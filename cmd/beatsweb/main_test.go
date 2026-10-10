package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unspok3n/beatportdl/config"
)

func testQueue(t *testing.T) *queue {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.AppConfig{Username: "user", Password: "pass", DownloadsDirectory: t.TempDir(), Quality: "lossless", KeySystem: "standard-short", TrackExists: "update"}
	if err := cfg.Save(filepath.Join(dir, "beatportdl-config.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beatportdl-credentials.json"), []byte(`{"access_token":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return newQueue("/bin/true", dir, map[string]string{"Downloads": t.TempDir()})
}

func TestFirstRunSetupStatusIsNotCached(t *testing.T) {
	dir := t.TempDir()
	q := newQueue("/bin/true", dir, map[string]string{"Downloads": t.TempDir()})
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/setup", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"configured":false`) {
		t.Fatalf("body = %s", res.Body.String())
	}
	if got := res.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestConfiguredStatus(t *testing.T) {
	q := testQueue(t)
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/setup", nil))
	if !strings.Contains(res.Body.String(), `"configured":true`) {
		t.Fatalf("body = %s", res.Body.String())
	}
}

func TestSettingsUpdatesQualityWithoutExposingCredentials(t *testing.T) {
	q := testQueue(t)
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"quality":"lossless"`) || strings.Contains(get.Body.String(), "pass") {
		t.Fatalf("GET settings status=%d body=%s", get.Code, get.Body.String())
	}

	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"quality":"medium"}`)))
	if post.Code != http.StatusOK {
		t.Fatalf("POST settings status=%d body=%s", post.Code, post.Body.String())
	}
	cfg, err := config.Parse(filepath.Join(q.workdir, "beatportdl-config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Quality != "medium" || cfg.Username != "user" || cfg.Password != "pass" {
		t.Fatalf("updated config = %#v", cfg)
	}
}

func TestJobsValidation(t *testing.T) {
	q := testQueue(t)
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"query":""}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", res.Code)
	}
}

func TestQueueCompletes(t *testing.T) {
	dir := t.TempDir()
	downloads := t.TempDir()
	cfg := &config.AppConfig{Username: "user", Password: "pass", DownloadsDirectory: downloads, Quality: "lossless", KeySystem: "standard-short", TrackExists: "update"}
	if err := cfg.Save(filepath.Join(dir, "beatportdl-config.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beatportdl-credentials.json"), []byte(`{"access_token":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	downloader := filepath.Join(t.TempDir(), "downloader")
	if err := os.WriteFile(downloader, []byte("#!/bin/sh\nprintf audio > \""+filepath.Join(downloads, "track.mp3")+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	q := newQueue(downloader, dir, map[string]string{"Downloads": downloads})
	j := q.add("https://www.beatport.com/track/example/1", "Downloads")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && q.list()[0].Status != "complete" {
		time.Sleep(time.Millisecond)
	}
	if j.Status != "complete" {
		t.Fatalf("status = %q, output = %q", j.Status, j.Output)
	}
}

func TestQueueFailsWhenDownloaderProducesNoAudio(t *testing.T) {
	q := testQueue(t)
	j := q.add("https://www.beatport.com/track/example/1", "Downloads")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && q.list()[0].Status != "failed" {
		time.Sleep(time.Millisecond)
	}
	if j.Status != "failed" || !strings.Contains(j.Output, "No audio file was created") {
		t.Fatalf("status = %q, output = %q", j.Status, j.Output)
	}
}

func TestQualityCandidatesFallBackFromMaximum(t *testing.T) {
	got := qualityCandidates("lossless")
	want := []string{"lossless", "high", "medium", "medium-hls"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("qualities = %#v", got)
	}
	got = qualityCandidates("medium")
	if strings.Join(got, ",") != "medium,medium-hls" {
		t.Fatalf("qualities = %#v", got)
	}
}

func TestConvertToMP3ReplacesSource(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "track.wav")
	command := exec.Command(ffmpeg, "-f", "lavfi", "-i", "sine=frequency=440:duration=0.1", "-y", source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, output)
	}
	if err := convertToMP3([]string{source}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "track.mp3"))
	if err != nil || info.Size() == 0 {
		t.Fatalf("MP3 missing or empty: %v", err)
	}
}

func TestParseFolders(t *testing.T) {
	got := parseFolders("Music=/srv/music,Archive=/mnt/archive,invalid")
	if got["Music"] != "/srv/music" || len(got) != 2 {
		t.Fatalf("folders = %#v", got)
	}
}

func TestLibraryListsAudioAndStreamsRanges(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "release"), 0700); err != nil {
		t.Fatal(err)
	}
	audio := []byte("0123456789")
	if err := os.WriteFile(filepath.Join(root, "release", "track.mp3"), audio, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ignore"), 0600); err != nil {
		t.Fatal(err)
	}
	q := newQueue("/bin/true", t.TempDir(), map[string]string{"Music": root})
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/library", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"path":"release/track.mp3"`) || strings.Contains(list.Body.String(), "notes.txt") {
		t.Fatalf("library status=%d body=%s", list.Code, list.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/media?folder=Music&path=release%2Ftrack.mp3", nil)
	request.Header.Set("Range", "bytes=2-5")
	stream := httptest.NewRecorder()
	h.ServeHTTP(stream, request)
	if stream.Code != http.StatusPartialContent || stream.Body.String() != "2345" {
		t.Fatalf("media status=%d body=%q", stream.Code, stream.Body.String())
	}
}

func TestMediaRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	q := newQueue("/bin/true", t.TempDir(), map[string]string{"Music": root})
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/media?folder=Music&path=..%2Fsecret.mp3", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d", res.Code)
	}
}

func TestArchiveDownloadAndConfirmedDeletion(t *testing.T) {
	root := t.TempDir()
	audioPath := filepath.Join(root, "track.mp3")
	if err := os.WriteFile(audioPath, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	q := newQueue("/bin/true", t.TempDir(), map[string]string{"Music": root})
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}

	created := httptest.NewRecorder()
	h.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/archive", nil))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var result struct {
		ID, Download string
		Files        int
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID == "" || result.Files != 1 {
		t.Fatalf("archive = %#v", result)
	}

	download := httptest.NewRecorder()
	h.ServeHTTP(download, httptest.NewRequest(http.MethodGet, result.Download, nil))
	if download.Code != http.StatusOK {
		t.Fatalf("download status=%d", download.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(download.Body.Bytes()), int64(download.Body.Len()))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "Music/track.mp3" {
		t.Fatalf("zip files=%v err=%v", zr.File, err)
	}
	if _, err := os.Stat(audioPath); err != nil {
		t.Fatalf("source deleted before confirmation: %v", err)
	}

	confirmed := httptest.NewRecorder()
	h.ServeHTTP(confirmed, httptest.NewRequest(http.MethodPost, "/api/archive/confirm?id="+result.ID, nil))
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	if _, err := os.Stat(audioPath); !os.IsNotExist(err) {
		t.Fatalf("source was not deleted: %v", err)
	}
}

func TestArchiveConfirmationRefusesChangedFiles(t *testing.T) {
	root := t.TempDir()
	audioPath := filepath.Join(root, "track.mp3")
	if err := os.WriteFile(audioPath, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	q := newQueue("/bin/true", t.TempDir(), map[string]string{"Music": root})
	h, err := handler(q)
	if err != nil {
		t.Fatal(err)
	}
	created := httptest.NewRecorder()
	h.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/archive", nil))
	var result struct{ ID string }
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audioPath, []byte("changed audio"), 0600); err != nil {
		t.Fatal(err)
	}
	confirmed := httptest.NewRecorder()
	h.ServeHTTP(confirmed, httptest.NewRequest(http.MethodPost, "/api/archive/confirm?id="+result.ID, nil))
	if confirmed.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	if _, err := os.Stat(audioPath); err != nil {
		t.Fatalf("changed file was deleted: %v", err)
	}
}
