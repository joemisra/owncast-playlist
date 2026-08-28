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

func TestConcatPositionTracksBoundariesAndLoops(t *testing.T) {
	inputs := []concatInput{
		{duration: 8},
		{duration: 10},
		{duration: 12},
	}
	tests := []struct {
		elapsed float64
		want    int
	}{
		{0, 0},
		{7.99, 0},
		{8, 1},
		{17.99, 1},
		{18, 2},
		{30, 0},
		{38, 1},
	}
	for _, test := range tests {
		if got := concatPosition(test.elapsed, inputs); got != test.want {
			t.Errorf("concatPosition(%v) = %d, want %d", test.elapsed, got, test.want)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	w := New(&config.Config{
		Owncast: config.OwncastConfig{StreamKey: "stream-secret"},
		Plex:    config.PlexConfig{Servers: []config.PlexServerConfig{{Token: "plex-secret"}}},
	})
	got := w.redactSecrets("rtmp stream-secret url?X-Plex-Token=plex-secret")
	if got != "rtmp [redacted] url?X-Plex-Token=[redacted]" {
		t.Fatalf("redacted line = %q", got)
	}
}
