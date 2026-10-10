// Beats Web adds a browser-based queue to BeatportDL.
package main

import (
	"archive/zip"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
		cfg, err := config.Parse(filepath.Join(q.workdir, "beatportdl-config.yml"))
		if err != nil {
			q.update(j, "failed", "load BeatportDL config: "+err.Error(), true)
			continue
		}
		var attempts []string
		finished := false
		for _, quality := range qualityCandidates(cfg.Quality) {
			runDir, cleanup, err := q.runDirectory(j.Folder, quality)
			if err != nil {
				attempts = append(attempts, quality+": "+err.Error())
				break
			}
			cmd := exec.Command(q.command, "-q", j.Query)
			cmd.Dir = runDir
			output, runErr := cmd.CombinedOutput()
			cleanup()
			message := strings.TrimSpace(string(output))
			if runErr != nil && message == "" {
				message = runErr.Error()
			}
			if message != "" {
				attempts = append(attempts, quality+": "+message)
			}
			after, scanErr := audioSnapshot(destination)
			if scanErr != nil {
				attempts = append(attempts, quality+": scan destination: "+scanErr.Error())
				break
			}
			changed := changedAudioFiles(before, after)
			if len(changed) == 0 {
				continue
			}
			if err := convertToMP3(changed); err != nil {
				q.update(j, "failed", "Audio downloaded at "+quality+" quality, but MP3 conversion failed: "+err.Error(), true)
				finished = true
				break
			}
			q.update(j, "complete", "Downloaded at "+quality+" quality and converted to 320 kbps MP3.", true)
			finished = true
			break
		}
		if !finished {
			detail := "No audio file was created at any allowed quality. Check the subscription and folder permissions."
			if len(attempts) > 0 {
				detail += "\n\n" + strings.Join(attempts, "\n\n")
			}
			q.update(j, "failed", detail, true)
		}
	}
}

func qualityCandidates(preferred string) []string {
	qualities := []string{"lossless", "high", "medium", "medium-hls"}
	for i, quality := range qualities {
		if quality == preferred {
			return qualities[i:]
		}
	}
	return qualities
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

func changedAudioFiles(before, after map[string]audioFileState) []string {
	var changed []string
	for path, state := range after {
		if previous, ok := before[path]; !ok || previous != state {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

func convertToMP3(files []string) error {
	for _, source := range files {
		if strings.EqualFold(filepath.Ext(source), ".mp3") {
			continue
		}
		target := strings.TrimSuffix(source, filepath.Ext(source)) + ".mp3"
		temporary, err := os.CreateTemp(filepath.Dir(source), ".beats-convert-*.mp3")
		if err != nil {
			return fmt.Errorf("prepare output for %s: %w", filepath.Base(source), err)
		}
		temporaryPath := temporary.Name()
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		output, err := exec.Command(
			"ffmpeg", "-nostdin", "-y", "-i", source,
			"-map", "0:a:0", "-map", "0:v?", "-map_metadata", "0",
			"-codec:a", "libmp3lame", "-b:a", "320k",
			"-codec:v", "copy", "-disposition:v:0", "attached_pic",
			"-id3v2_version", "3", "-write_id3v1", "1",
			temporaryPath,
		).CombinedOutput()
		if err != nil {
			_ = os.Remove(temporaryPath)
			return fmt.Errorf("convert %s: %w: %s", filepath.Base(source), err, strings.TrimSpace(string(output)))
		}
		info, err := os.Stat(temporaryPath)
		if err != nil || info.Size() == 0 {
			_ = os.Remove(temporaryPath)
			return fmt.Errorf("convert %s: FFmpeg produced no audio", filepath.Base(source))
		}
		if err := os.Rename(temporaryPath, target); err != nil {
			_ = os.Remove(temporaryPath)
			return fmt.Errorf("save %s: %w", filepath.Base(target), err)
		}
		if err := os.Remove(source); err != nil {
			return fmt.Errorf("MP3 saved but original %s could not be removed: %w", filepath.Base(source), err)
		}
	}
	return nil
}

func (q *queue) runDirectory(folder, quality string) (string, func(), error) {
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
	cfg.Quality = quality
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
	queue         *queue
	workdir       string
	setupMu       *sync.Mutex
	archiveMu     *sync.Mutex
	metadataMu    *sync.Mutex
	metadataCache map[string]cachedMetadata
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
	Title    string `json:"title,omitempty"`
	Artist   string `json:"artist,omitempty"`
	Album    string `json:"album,omitempty"`
	Genre    string `json:"genre,omitempty"`
	Date     string `json:"date,omitempty"`
	Track    string `json:"track,omitempty"`
	Disc     string `json:"disc,omitempty"`
	Label    string `json:"label,omitempty"`
	Catalog  string `json:"catalog,omitempty"`
	ISRC     string `json:"isrc,omitempty"`
	Artwork  bool   `json:"artwork"`
}

type audioMetadata struct {
	Title, Artist, Album, Genre, Date, Track, Disc, Label, Catalog, ISRC string
	Artwork                                                              bool
}

type cachedMetadata struct {
	state audioFileState
	data  audioMetadata
}

func firstTag(tags map[string]string, names ...string) string {
	for _, name := range names {
		if value := tags[strings.ToLower(name)]; value != "" {
			return value
		}
	}
	return ""
}

func probeMetadata(path string) audioMetadata {
	output, err := exec.Command("ffprobe", "-v", "error", "-show_format", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		return audioMetadata{}
	}
	var probe struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			CodecType   string `json:"codec_type"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if json.Unmarshal(output, &probe) != nil {
		return audioMetadata{}
	}
	tags := make(map[string]string, len(probe.Format.Tags))
	for key, value := range probe.Format.Tags {
		tags[strings.ToLower(key)] = value
	}
	metadata := audioMetadata{
		Title: firstTag(tags, "title"), Artist: firstTag(tags, "artist"), Album: firstTag(tags, "album"),
		Genre: firstTag(tags, "genre"), Date: firstTag(tags, "date", "year"), Track: firstTag(tags, "track"),
		Disc: firstTag(tags, "disc"), Label: firstTag(tags, "label", "publisher"),
		Catalog: firstTag(tags, "catalognumber", "catalog_number"), ISRC: firstTag(tags, "isrc"),
	}
	for _, stream := range probe.Streams {
		if stream.CodecType == "video" && stream.Disposition.AttachedPic == 1 {
			metadata.Artwork = true
			break
		}
	}
	return metadata
}

func (s server) metadata(path string, state audioFileState) audioMetadata {
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	if cached, ok := s.metadataCache[path]; ok && cached.state == state {
		return cached.data
	}
	data := probeMetadata(path)
	s.metadataCache[path] = cachedMetadata{state: state, data: data}
	return data
}

type archiveEntry struct {
	Folder  string `json:"folder"`
	Root    string `json:"root"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

type archiveManifest struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Created string         `json:"created"`
	Entries []archiveEntry `json:"entries"`
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
			metadata := s.metadata(path, audioFileState{size: info.Size(), modTime: info.ModTime().UnixNano()})
			item := &items[len(items)-1]
			item.Title, item.Artist, item.Album, item.Genre, item.Date = metadata.Title, metadata.Artist, metadata.Album, metadata.Genre, metadata.Date
			item.Track, item.Disc, item.Label, item.Catalog, item.ISRC, item.Artwork = metadata.Track, metadata.Disc, metadata.Label, metadata.Catalog, metadata.ISRC, metadata.Artwork
			return nil
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Modified > items[j].Modified })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func archiveID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func validArchiveID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (s server) archivesDir() string { return filepath.Join(s.workdir, "exports") }

func (s server) createArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	id, err := archiveID()
	if err != nil {
		http.Error(w, `{"error":"Could not create archive"}`, http.StatusInternalServerError)
		return
	}
	dir := s.archivesDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		http.Error(w, `{"error":"Could not prepare archive storage"}`, http.StatusInternalServerError)
		return
	}
	name := "beats-library-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	zipPath := filepath.Join(dir, id+".zip")
	temporary, err := os.CreateTemp(dir, ".beats-archive-*.zip")
	if err != nil {
		http.Error(w, `{"error":"Could not create archive"}`, http.StatusInternalServerError)
		return
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = temporary.Close(); _ = os.Remove(temporaryPath) }
	zw := zip.NewWriter(temporary)
	manifest := archiveManifest{ID: id, Name: name, Created: time.Now().UTC().Format(time.RFC3339)}
	for folder, root := range s.queue.folders {
		resolvedRoot, resolveErr := filepath.EvalSymlinks(root)
		if errors.Is(resolveErr, os.ErrNotExist) {
			continue
		}
		if resolveErr != nil {
			err = resolveErr
			break
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isAudio(path) {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			source, err := os.Open(path)
			if err != nil {
				return err
			}
			folderName := strings.NewReplacer("/", "_", "\\", "_").Replace(folder)
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				_ = source.Close()
				return err
			}
			header.Name = filepath.ToSlash(filepath.Join(folderName, relative))
			header.Method = zip.Store
			destination, err := zw.CreateHeader(header)
			if err == nil {
				_, err = io.Copy(destination, source)
			}
			_ = source.Close()
			if err != nil {
				return err
			}
			manifest.Entries = append(manifest.Entries, archiveEntry{Folder: folder, Root: resolvedRoot, Path: filepath.ToSlash(relative), Size: info.Size(), ModTime: info.ModTime().UnixNano()})
			return nil
		})
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			break
		}
	}
	if err == nil && len(manifest.Entries) == 0 {
		err = errors.New("library is empty")
	}
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		http.Error(w, `{"error":"Could not archive the library: `+strings.ReplaceAll(err.Error(), `"`, "")+`"}`, http.StatusUnprocessableEntity)
		return
	}
	if err := os.Rename(temporaryPath, zipPath); err != nil {
		cleanup()
		http.Error(w, `{"error":"Could not save archive"}`, http.StatusInternalServerError)
		return
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil || os.WriteFile(filepath.Join(dir, id+".json"), manifestData, 0600) != nil {
		_ = os.Remove(zipPath)
		http.Error(w, `{"error":"Could not save archive manifest"}`, http.StatusInternalServerError)
		return
	}
	info, _ := os.Stat(zipPath)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": name, "size": info.Size(), "files": len(manifest.Entries), "download": "/api/archive/download?id=" + id})
}

func (s server) downloadArchive(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !validArchiveID(id) {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.archivesDir(), id+".json"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var manifest archiveManifest
	if json.Unmarshal(data, &manifest) != nil || manifest.ID != id {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(filepath.Join(s.archivesDir(), id+".zip"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+manifest.Name+`"`)
	http.ServeContent(w, r, manifest.Name, info.ModTime(), file)
}

func (s server) confirmArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	id := r.URL.Query().Get("id")
	if !validArchiveID(id) {
		http.NotFound(w, r)
		return
	}
	manifestPath := filepath.Join(s.archivesDir(), id+".json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var manifest archiveManifest
	if json.Unmarshal(data, &manifest) != nil || manifest.ID != id {
		http.NotFound(w, r)
		return
	}
	targets := make([]string, 0, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		root, ok := s.queue.folders[entry.Folder]
		relative := filepath.Clean(filepath.FromSlash(entry.Path))
		if !ok || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			http.Error(w, `{"error":"Archive manifest is no longer safe"}`, http.StatusConflict)
			return
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil || resolvedRoot != entry.Root {
			http.Error(w, `{"error":"A library folder changed after the ZIP was created; nothing was deleted"}`, http.StatusConflict)
			return
		}
		target, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, relative))
		if err != nil || !strings.HasPrefix(target, resolvedRoot+string(filepath.Separator)) {
			http.Error(w, `{"error":"A library file changed after the ZIP was created; nothing was deleted"}`, http.StatusConflict)
			return
		}
		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size || info.ModTime().UnixNano() != entry.ModTime {
			http.Error(w, `{"error":"A library file changed after the ZIP was created; nothing was deleted"}`, http.StatusConflict)
			return
		}
		targets = append(targets, target)
	}
	for _, target := range targets {
		if err := os.Remove(target); err != nil {
			http.Error(w, `{"error":"Could not delete all archived files"}`, http.StatusInternalServerError)
			return
		}
	}
	_ = os.Remove(filepath.Join(s.archivesDir(), id+".zip"))
	_ = os.Remove(manifestPath)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"deleted": len(targets)})
}

func (s server) media(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resolvedFile, err := resolveLibraryFile(s.queue.folders, r.URL.Query().Get("folder"), r.URL.Query().Get("path"))
	if err != nil {
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

func resolveLibraryFile(folders map[string]string, folder, requestedPath string) (string, error) {
	root, ok := folders[folder]
	relative := filepath.Clean(filepath.FromSlash(requestedPath))
	if !ok || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || !isAudio(relative) {
		return "", errors.New("invalid library path")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedFile, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, relative))
	if err != nil || !strings.HasPrefix(resolvedFile, resolvedRoot+string(filepath.Separator)) {
		return "", errors.New("library path escapes its folder")
	}
	return resolvedFile, nil
}

func (s server) artwork(w http.ResponseWriter, r *http.Request) {
	resolvedFile, err := resolveLibraryFile(s.queue.folders, r.URL.Query().Get("folder"), r.URL.Query().Get("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	output, err := exec.Command("ffmpeg", "-v", "error", "-i", resolvedFile, "-map", "0:v:0", "-frames:v", "1", "-f", "image2pipe", "-codec:v", "mjpeg", "pipe:1").Output()
	if err != nil || len(output) == 0 || len(output) > 10<<20 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(output)))
	_, _ = w.Write(output)
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
	s := server{queue: q, workdir: q.workdir, setupMu: &sync.Mutex{}, archiveMu: &sync.Mutex{}, metadataMu: &sync.Mutex{}, metadataCache: make(map[string]cachedMetadata)}
	mux.HandleFunc("/api/setup", s.setup)
	mux.HandleFunc("/api/settings", s.settings)
	mux.HandleFunc("/api/jobs", s.jobs)
	mux.HandleFunc("/api/folders", s.folders)
	mux.HandleFunc("/api/search", s.search)
	mux.HandleFunc("/api/library", s.library)
	mux.HandleFunc("/api/media", s.media)
	mux.HandleFunc("/api/artwork", s.artwork)
	mux.HandleFunc("/api/archive", s.createArchive)
	mux.HandleFunc("/api/archive/download", s.downloadArchive)
	mux.HandleFunc("/api/archive/confirm", s.confirmArchive)
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
