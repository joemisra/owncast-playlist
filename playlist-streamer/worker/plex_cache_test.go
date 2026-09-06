package worker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlexCacheDownloadsAndReusesCompleteFile(t *testing.T) {
	payload := bytes.Repeat([]byte("media-data-"), 20_000)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.Write(payload)
	}))
	defer server.Close()

	cache := testPlexCache(t, server.Client())
	protected := map[string]bool{}
	path, err := cache.fetch(context.Background(), "plex://home/42", server.URL+"/file.mkv?X-Plex-Token=secret", protected, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("cached payload mismatch: bytes=%d err=%v", len(got), err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d, want 1", requests.Load())
	}
	if _, err := cache.fetch(context.Background(), "plex://home/42", server.URL+"/file.mkv?X-Plex-Token=secret", protected, nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("cache hit made another request; requests=%d", requests.Load())
	}
}

func TestPlexCacheReceivesAtomicPublicUpload(t *testing.T) {
	payload := bytes.Repeat([]byte("public-upload-"), 10_000)
	cache := testPlexCache(t, http.DefaultClient)
	key := "smb://movies/Example/movie.mkv"
	var received, total int64

	path, err := cache.receive(context.Background(), key, bytes.NewReader(payload), int64(len(payload)), func(current, size int64) {
		received, total = current, size
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("uploaded cache payload mismatch: bytes=%d err=%v", len(got), err)
	}
	if received != int64(len(payload)) || total != int64(len(payload)) {
		t.Fatalf("progress=(%d,%d), want (%d,%d)", received, total, len(payload), len(payload))
	}
	if _, err := os.Stat(path + ".incoming"); !os.IsNotExist(err) {
		t.Fatalf("incoming file remains after upload: %v", err)
	}
}

func TestPlexCacheResumesPartialFile(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 30_000)
	const offset = 70_000
	var rangeHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader = r.Header.Get("Range")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)-offset))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[offset:])
	}))
	defer server.Close()

	cache := testPlexCache(t, server.Client())
	finalPath := cache.path("plex://home/99", server.URL+"/file.mp4")
	if err := os.WriteFile(finalPath+".partial", payload[:offset], 0o640); err != nil {
		t.Fatal(err)
	}
	var progressReceived, progressTotal int64
	path, err := cache.fetch(context.Background(), "plex://home/99", server.URL+"/file.mp4", map[string]bool{}, func(received, total int64) {
		progressReceived, progressTotal = received, total
	})
	if err != nil {
		t.Fatal(err)
	}
	if rangeHeader != fmt.Sprintf("bytes=%d-", offset) {
		t.Fatalf("Range=%q", rangeHeader)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed payload mismatch: got %d bytes", len(got))
	}
	if progressReceived != int64(len(payload)) || progressTotal != int64(len(payload)) {
		t.Fatalf("progress=%d/%d", progressReceived, progressTotal)
	}
}

func TestPlexCacheEvictsOldUnprotectedFile(t *testing.T) {
	cache := testPlexCache(t, http.DefaultClient)
	cache.maxBytes = 250_000
	oldPath := filepath.Join(cache.dir, "old.mkv")
	if err := os.WriteFile(oldPath, make([]byte, 120_000), 0o640); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := cache.ensureCapacity(200_000, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old cache entry was not evicted: %v", err)
	}
}

func TestPlexCacheErrorDoesNotExposeToken(t *testing.T) {
	const token = "private-plex-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "failure "+token, http.StatusBadGateway)
	}))
	defer server.Close()

	cache := testPlexCache(t, server.Client())
	_, err := cache.fetch(context.Background(), "plex://home/7", server.URL+"/file.mkv?X-Plex-Token="+token, map[string]bool{}, nil)
	if err == nil {
		t.Fatal("expected download failure")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in error: %v", err)
	}
	if !isTemporaryPlexCacheError(err) {
		t.Fatalf("HTTP 502 should be retriable: %v", err)
	}
}

func TestMediaCacheCopiesAndResumesSMBFile(t *testing.T) {
	payload := bytes.Repeat([]byte("smb-media-"), 30_000)
	source := filepath.Join(t.TempDir(), "Episode 01.mkv")
	if err := os.WriteFile(source, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	cache := testPlexCache(t, http.DefaultClient)
	finalPath := cache.path("smb://tv/Show/Episode%2001.mkv", source)
	const offset = 80_000
	if err := os.WriteFile(finalPath+".partial", payload[:offset], 0o640); err != nil {
		t.Fatal(err)
	}

	var received, total int64
	path, err := cache.fetchFile(context.Background(), "smb://tv/Show/Episode%2001.mkv", source, map[string]bool{}, func(current, size int64) {
		received, total = current, size
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("cached SMB payload mismatch: bytes=%d err=%v", len(got), err)
	}
	if received != int64(len(payload)) || total != int64(len(payload)) {
		t.Fatalf("progress=%d/%d, want %d/%d", received, total, len(payload), len(payload))
	}
	if _, err := os.Stat(finalPath + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial cache file remains: %v", err)
	}
	if _, err := cache.fetchFile(context.Background(), "smb://tv/Show/Episode%2001.mkv", source, map[string]bool{}, nil); err != nil {
		t.Fatal(err)
	}
}

func testPlexCache(t *testing.T, client *http.Client) *plexCache {
	t.Helper()
	return &plexCache{
		dir:          t.TempDir(),
		maxBytes:     10 * 1024 * 1024,
		minFreeBytes: 0,
		client:       client,
	}
}
