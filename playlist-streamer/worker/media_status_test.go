package worker

import (
	"testing"

	"playlist-streamer/playlist"
)

func TestMediaStatusResetLabelsCachedAndDirectSources(t *testing.T) {
	var store mediaStatusStore
	store.reset(&playlist.Playlist{Videos: []playlist.VideoEntry{
		{URL: "/videos/local.mkv", Provider: "local"},
		{URL: "smb://Movies/show/episode.mkv", Provider: "smb"},
		{URL: "https://example.test/live.m3u8", Provider: "http"},
	}}, true)

	statuses := store.snapshot()
	if statuses["/videos/local.mkv"].State != "ready" {
		t.Fatalf("local state = %q, want ready", statuses["/videos/local.mkv"].State)
	}
	if statuses["smb://Movies/show/episode.mkv"].State != "waiting" {
		t.Fatalf("SMB state = %q, want waiting", statuses["smb://Movies/show/episode.mkv"].State)
	}
	if statuses["https://example.test/live.m3u8"].State != "direct" {
		t.Fatalf("HTTP state = %q, want direct", statuses["https://example.test/live.m3u8"].State)
	}
}

func TestMediaStatusProgressAndSnapshotIsolation(t *testing.T) {
	var store mediaStatusStore
	store.set("smb://Movies/movie.mkv", "caching", "Transferring to Couch", 25, 100)

	statuses := store.snapshot()
	status := statuses["smb://Movies/movie.mkv"]
	if status.State != "caching" || status.Bytes != 25 || status.Total != 100 {
		t.Fatalf("unexpected progress status: %#v", status)
	}
	delete(statuses, "smb://Movies/movie.mkv")
	if _, ok := store.snapshot()["smb://Movies/movie.mkv"]; !ok {
		t.Fatal("mutating a snapshot changed the store")
	}
}
