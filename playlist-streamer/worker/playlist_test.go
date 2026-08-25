package worker

import (
	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"testing"
)

func testWorkerWithURLs(urls ...string) *StreamWorker {
	w := New(&config.Config{})
	videos := make([]playlist.VideoEntry, len(urls))
	for i, u := range urls {
		videos[i] = playlist.VideoEntry{URL: u, Provider: "local"}
	}
	w.playlist = &playlist.Playlist{Name: "test", Videos: videos}
	return w
}

func TestMoveVideo(t *testing.T) {
	w := testWorkerWithURLs("a", "b", "c", "d")
	w.index.Store(0)
	if restarted, err := w.MoveVideo(3, 1); err != nil || restarted {
		t.Fatalf("move: restarted=%v err=%v", restarted, err)
	}
	got := w.CurrentPlaylist().Videos
	want := []string{"a", "d", "b", "c"}
	for i := range want {
		if got[i].URL != want[i] {
			t.Fatalf("index %d: got %q want %q", i, got[i].URL, want[i])
		}
	}
}

func TestRemoveBeforeCurrentAdjustsCursor(t *testing.T) {
	w := testWorkerWithURLs("a", "b", "c")
	w.index.Store(2)
	if err := w.RemoveVideo(0); err != nil {
		t.Fatal(err)
	}
	if got := w.CurrentIndex(); got != 1 {
		t.Fatalf("current index=%d, want 1", got)
	}
	if got := w.CurrentPlaylist().Videos[1].URL; got != "c" {
		t.Fatalf("current item=%q, want c", got)
	}
}
