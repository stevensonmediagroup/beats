package main

import (
	"net/http"
	"net/http/httptest"
	"os"
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
	q := testQueue(t)
	j := q.add("https://www.beatport.com/track/example/1", "Downloads")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && q.list()[0].Status != "complete" {
		time.Sleep(time.Millisecond)
	}
	if j.Status != "complete" {
		t.Fatalf("status = %q, output = %q", j.Status, j.Output)
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
