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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		q.update(j, "complete", message, true)
	}
}

func (q *queue) runDirectory(folder string) (string, func(), error) {
	destination, ok := q.folders[folder]
	if !ok {
		return "", func() {}, fmt.Errorf("unknown destination folder %q", folder)
	}
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
}

func (s server) jobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(s.queue.list())
	case http.MethodPost:
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
	s := server{queue: q, workdir: q.workdir}
	mux.HandleFunc("/api/jobs", s.jobs)
	mux.HandleFunc("/api/folders", s.folders)
	mux.HandleFunc("/api/search", s.search)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return mux, nil
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
