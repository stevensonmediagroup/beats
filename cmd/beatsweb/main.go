// Beats Web adds a browser-based queue to BeatportDL.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unspok3n/beatportdl/config"
	"unspok3n/beatportdl/internal/beatport"
)

//go:embed web/*
var webFiles embed.FS

type job struct {
	ID       string `json:"id"`
	Query    string `json:"query"`
	Folder   string `json:"folder"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	Created  string `json:"created"`
	Finished string `json:"finished,omitempty"`
}

type queue struct {
	mu      sync.RWMutex
	jobs    []*job
	pending chan *job
	command string
	workdir string
	folders map[string]string
}

func newQueue(command, workdir string, folders map[string]string) *queue {
	q := &queue{pending: make(chan *job, 100), command: command, workdir: workdir, folders: folders}
	go q.worker()
	return q
}

func (q *queue) add(query, folder string) *job {
	j := &job{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Query: query, Folder: folder, Status: "queued", Created: time.Now().UTC().Format(time.RFC3339)}
	q.mu.Lock()
	q.jobs = append([]*job{j}, q.jobs...)
	q.mu.Unlock()
	q.pending <- j
	return j
}

func (q *queue) list() []*job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	result := make([]*job, len(q.jobs))
	for i, item := range q.jobs {
		copy := *item
		result[i] = &copy
	}
	return result
}

func (q *queue) worker() {
	for j := range q.pending {
		q.update(j, "downloading", "", false)
		destination := q.folders[j.Folder]
		before, _ := audioSnapshot(destination)
		runDir, cleanup, err := q.runDirectory(j.Folder)
		if err != nil {
			q.update(j, "failed", err.Error(), true)
			continue
		}
		cmd := exec.Command(q.command, "-q", j.Query)
		cmd.Dir = runDir
		output, err := cmd.CombinedOutput()
		cleanup()
		message := strings.TrimSpace(string(output))
		if err != nil {
			if message == "" {
				message = err.Error()
			}
			q.update(j, "failed", message, true)
			continue
		}
		after, scanErr := audioSnapshot(destination)
		if scanErr != nil {
			q.update(j, "failed", "Downloader exited but the destination could not be scanned: "+scanErr.Error(), true)
			continue
		}
		if !snapshotChanged(before, after) {
			detail := "Downloader exited successfully but no new or updated audio file was found in the selected folder. Check the account subscription, quality, and folder permissions."
			if message != "" {
				detail += "\n\n" + message
			}
			q.update(j, "failed", detail, true)
			continue
		}
		q.update(j, "complete", message, true)
	}
}

type audioFileState struct {
	size    int64
	modTime int64
}

func audioSnapshot(root string) (map[string]audioFileState, error) {
	result := make(map[string]audioFileState)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isAudio(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			result[path] = audioFileState{size: info.Size(), modTime: info.ModTime().UnixNano()}
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	return result, err
}

func snapshotChanged(before, after map[string]audioFileState) bool {
	for path, state := range after {
		if previous, ok := before[path]; !ok || previous != state {
			return true
		}
	}
	return false
}

func (q *queue) runDirectory(folder string) (string, func(), error) {
	destination, ok := q.folders[folder]
	if !ok {
		return "", func() {}, fmt.Errorf("unknown destination folder %q", folder)
	}
	if err := os.MkdirAll(destination, 0755); err != nil {
		return "", func() {}, fmt.Errorf("prepare destination folder: %w", err)
	}
	probe, err := os.CreateTemp(destination, ".beats-write-test-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("destination folder is not writable: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return "", func() {}, fmt.Errorf("test destination folder: %w", err)
	}
	_ = os.Remove(probePath)
	cfg, err := config.Parse(filepath.Join(q.workdir, "beatportdl-config.yml"))
	if err != nil {
		return "", func() {}, fmt.Errorf("load BeatportDL config: %w", err)
	}
	cfg.DownloadsDirectory = destination
	dir, err := os.MkdirTemp("", "beats-job-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := cfg.Save(filepath.Join(dir, "beatportdl-config.yml")); err != nil {
		cleanup()
		return "", func() {}, err
	}
	cacheName := "beatportdl-credentials.json"
	sourceCache := filepath.Join(q.workdir, cacheName)
	temporaryCache := filepath.Join(dir, cacheName)
	if data, err := os.ReadFile(sourceCache); err == nil {
		if err := os.WriteFile(temporaryCache, data, 0600); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	cleanup = func() {
		if data, err := os.ReadFile(temporaryCache); err == nil {
			_ = os.WriteFile(sourceCache, data, 0600)
		}
		_ = os.RemoveAll(dir)
	}
	return dir, cleanup, nil
}

func (q *queue) update(j *job, status, output string, finished bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j.Status, j.Output = status, output
	if finished {
		j.Finished = time.Now().UTC().Format(time.RFC3339)
	}
}

type server struct {
	queue   *queue
	workdir string
	setupMu *sync.Mutex
}

func validQuality(quality string) bool {
	return quality == "lossless" || quality == "high" || quality == "medium" || quality == "medium-hls"
}

func (s server) configured() bool {
	if _, err := config.Parse(filepath.Join(s.workdir, "beatportdl-config.yml")); err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(s.workdir, "beatportdl-credentials.json"))
	return err == nil && !info.IsDir() && info.Size() > 0
}

func (s server) setup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]bool{"configured": s.configured()})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.configured() {
		http.Error(w, `{"error":"Beats is already configured"}`, http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Quality  string `json:"quality"`
		Folder   string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"Invalid setup request"}`, http.StatusBadRequest)
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" || input.Password == "" {
		http.Error(w, `{"error":"Beatport username and password are required"}`, http.StatusBadRequest)
		return
	}
	if !validQuality(input.Quality) {
		http.Error(w, `{"error":"Select a valid audio quality"}`, http.StatusBadRequest)
		return
	}
	destination, ok := s.queue.folders[input.Folder]
	if !ok {
		http.Error(w, `{"error":"Select a valid destination folder"}`, http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(s.workdir, 0700); err != nil {
		http.Error(w, `{"error":"Could not prepare the data directory"}`, http.StatusInternalServerError)
		return
	}

	cacheTemp, err := os.CreateTemp(s.workdir, ".beatportdl-credentials-*")
	if err != nil {
		http.Error(w, `{"error":"Could not prepare credentials"}`, http.StatusInternalServerError)
		return
	}
	cachePath := cacheTemp.Name()
	_ = cacheTemp.Close()
	_ = os.Remove(cachePath)
	defer os.Remove(cachePath)

	auth := beatport.NewAuth(input.Username, input.Password, cachePath)
	bp := beatport.New("", auth)
	if err := auth.Init(bp); err != nil {
		http.Error(w, `{"error":"Beatport sign-in failed; check the account and subscription"}`, http.StatusUnauthorized)
		return
	}

	cfg := &config.AppConfig{
		Username: input.Username, Password: input.Password, Quality: input.Quality,
		DownloadsDirectory: destination,
	}
	configTemp, err := os.CreateTemp(s.workdir, ".beatportdl-config-*")
	if err != nil {
		http.Error(w, `{"error":"Could not prepare configuration"}`, http.StatusInternalServerError)
		return
	}
	configPath := configTemp.Name()
	_ = configTemp.Close()
	_ = os.Remove(configPath)
	defer os.Remove(configPath)
	if err := cfg.Save(configPath); err != nil || os.Chmod(configPath, 0600) != nil {
		http.Error(w, `{"error":"Could not save configuration"}`, http.StatusInternalServerError)
		return
	}
	if err := os.Rename(cachePath, filepath.Join(s.workdir, "beatportdl-credentials.json")); err != nil {
		http.Error(w, `{"error":"Could not save credentials"}`, http.StatusInternalServerError)
		return
	}
	if err := os.Rename(configPath, filepath.Join(s.workdir, "beatportdl-config.yml")); err != nil {
		_ = os.Remove(filepath.Join(s.workdir, "beatportdl-credentials.json"))
		http.Error(w, `{"error":"Could not save configuration"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"configured":true}`))
}

func (s server) settings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	cfgPath := filepath.Join(s.workdir, "beatportdl-config.yml")
	cfg, err := config.Parse(cfgPath)
	if err != nil {
		http.Error(w, `{"error":"Complete first-run setup before changing settings"}`, http.StatusPreconditionFailed)
		return
	}
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]string{"quality": cfg.Quality})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Quality string `json:"quality"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input); err != nil || !validQuality(input.Quality) {
		http.Error(w, `{"error":"Select a valid audio quality"}`, http.StatusBadRequest)
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	cfg, err = config.Parse(cfgPath)
	if err != nil {
		http.Error(w, `{"error":"Could not load settings"}`, http.StatusInternalServerError)
		return
	}
	cfg.Quality = input.Quality
	temporary, err := os.CreateTemp(s.workdir, ".beatportdl-config-*")
	if err != nil {
		http.Error(w, `{"error":"Could not save settings"}`, http.StatusInternalServerError)
		return
	}
	temporaryPath := temporary.Name()
	_ = temporary.Close()
	_ = os.Remove(temporaryPath)
	defer os.Remove(temporaryPath)
	if err := cfg.Save(temporaryPath); err != nil || os.Chmod(temporaryPath, 0600) != nil || os.Rename(temporaryPath, cfgPath) != nil {
		http.Error(w, `{"error":"Could not save settings"}`, http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"quality": cfg.Quality})
}

func (s server) jobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(s.queue.list())
	case http.MethodPost:
		if !s.configured() {
			http.Error(w, `{"error":"Complete first-run setup before adding downloads"}`, http.StatusPreconditionFailed)
			return
		}
		var input struct {
			Query  string `json:"query"`
			Folder string `json:"folder"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.Query) == "" {
			http.Error(w, `{"error":"A Beatport/Beatsource URL or search query is required"}`, http.StatusBadRequest)
			return
		}
		if _, ok := s.queue.folders[input.Folder]; !ok {
			http.Error(w, `{"error":"Select a valid destination folder"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(s.queue.add(strings.TrimSpace(input.Query), input.Folder))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s server) folders(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	names := make([]string, 0, len(s.queue.folders))
	for name := range s.queue.folders {
		names = append(names, name)
	}
	_ = json.NewEncoder(w).Encode(names)
}

type libraryItem struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Folder   string `json:"folder"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

func isAudio(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac", ".m4a", ".aac", ".mp3", ".ogg", ".opus", ".wav":
		return true
	default:
		return false
	}
}

func (s server) library(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	items := make([]libraryItem, 0)
	for folder, root := range s.queue.folders {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if len(items) >= 5000 {
				return fs.SkipAll
			}
			if err != nil {
				return nil
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isAudio(path) {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			items = append(items, libraryItem{
				Name: entry.Name(), Path: filepath.ToSlash(relative), Folder: folder,
				Size: info.Size(), Modified: info.ModTime().UTC().Format(time.RFC3339),
			})
			return nil
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Modified > items[j].Modified })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func (s server) media(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, ok := s.queue.folders[r.URL.Query().Get("folder")]
	relative := filepath.Clean(filepath.FromSlash(r.URL.Query().Get("path")))
	if !ok || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || !isAudio(relative) {
		http.NotFound(w, r)
		return
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resolvedFile, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, relative))
	if err != nil || !strings.HasPrefix(resolvedFile, resolvedRoot+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(resolvedFile)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(resolvedFile)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(info.Name(), `"`, "")+`"`)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s server) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, `{"error":"Search query is required"}`, http.StatusBadRequest)
		return
	}
	cfg, err := config.Parse(filepath.Join(s.workdir, "beatportdl-config.yml"))
	if err != nil {
		http.Error(w, `{"error":"Configure BeatportDL before searching"}`, http.StatusPreconditionFailed)
		return
	}
	auth := beatport.NewAuth(cfg.Username, cfg.Password, filepath.Join(s.workdir, "beatportdl-credentials.json"))
	bp := beatport.New(cfg.Proxy, auth)
	if err := auth.LoadCache(); err != nil {
		if err := auth.Init(bp); err != nil {
			http.Error(w, `{"error":"Beatport authentication failed"}`, http.StatusUnauthorized)
			return
		}
	}
	results, err := bp.Search(query)
	if err != nil {
		http.Error(w, `{"error":"Beatport search failed"}`, http.StatusBadGateway)
		return
	}
	type item struct{ Type, Name, Artist, URL, Image string }
	items := make([]item, 0, len(results.Tracks)+len(results.Releases))
	for i := range results.Tracks {
		t := &results.Tracks[i]
		items = append(items, item{"track", t.Name.String(), t.Artists.Display(99, ""), t.StoreUrl(), ""})
	}
	for i := range results.Releases {
		rel := &results.Releases[i]
		items = append(items, item{"release", rel.Name.String(), rel.Artists.Display(99, ""), rel.StoreUrl(), ""})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func handler(q *queue) (http.Handler, error) {
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	s := server{queue: q, workdir: q.workdir, setupMu: &sync.Mutex{}}
	mux.HandleFunc("/api/setup", s.setup)
	mux.HandleFunc("/api/settings", s.settings)
	mux.HandleFunc("/api/jobs", s.jobs)
	mux.HandleFunc("/api/folders", s.folders)
	mux.HandleFunc("/api/search", s.search)
	mux.HandleFunc("/api/library", s.library)
	mux.HandleFunc("/api/media", s.media)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return noStore(mux), nil
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("addr", env("BEATS_ADDR", "127.0.0.1:8080"), "HTTP listen address")
	command := flag.String("downloader", env("BEATPORTDL_BIN", "./beatportdl"), "BeatportDL executable")
	workdir := flag.String("workdir", env("BEATS_DATA_DIR", "."), "directory containing BeatportDL config and downloads")
	flag.Parse()

	absWorkdir, err := filepath.Abs(*workdir)
	if err != nil {
		log.Fatal(err)
	}
	absCommand, err := filepath.Abs(*command)
	if err != nil {
		log.Fatal(err)
	}
	if info, err := os.Stat(absCommand); err != nil || info.IsDir() {
		log.Fatalf("downloader %q is unavailable; build it first (make linux-amd64)", *command)
	}
	folders := parseFolders(env("BEATS_FOLDERS", "Downloads="+filepath.Join(absWorkdir, "downloads")))
	h, err := handler(newQueue(absCommand, absWorkdir, folders))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Beats Web listening on http://%s", *addr)
	if err := http.ListenAndServe(*addr, h); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func parseFolders(value string) map[string]string {
	result := map[string]string{}
	for _, entry := range strings.Split(value, ",") {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && filepath.IsAbs(strings.TrimSpace(parts[1])) {
			result[strings.TrimSpace(parts[0])] = filepath.Clean(strings.TrimSpace(parts[1]))
		}
	}
	return result
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
