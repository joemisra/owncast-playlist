package worker

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/providers"
)

// StreamWorker streams videos from a playlist to Owncast via RTMP.
type StreamWorker struct {
	cfg      *config.Config
	registry *providers.Registry
}

// New creates a new stream worker.
func New(cfg *config.Config) *StreamWorker {
	reg := providers.NewRegistry()
	if yt, ok := reg.Get("youtube").(*providers.YouTube); ok && yt != nil {
		yt.SetYtdlpPath(cfg.Streamer.YtdlpPath)
	}
	return &StreamWorker{
		cfg:      cfg,
		registry: reg,
	}
}

// StreamPlaylist streams all videos in the playlist to Owncast.
// On each video end, advances to the next. If loop_playlist is true, starts over when done.
func (w *StreamWorker) StreamPlaylist(ctx context.Context, pl *playlist.Playlist) error {
	idx := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if idx >= len(pl.Videos) {
			if !w.cfg.Streamer.LoopPlaylist {
				return nil
			}
			idx = 0
		}
		entry := pl.Videos[idx]
		provider := w.registry.Get(entry.Provider)
		if provider == nil {
			return fmt.Errorf("unknown provider: %s", entry.Provider)
		}
		streamURL, err := provider.GetStreamURL(entry.URL)
		if err != nil {
			if err == providers.ErrNotImplemented {
				idx++
				continue
			}
			return fmt.Errorf("get stream url for %s: %w", entry.URL, err)
		}
		if err := w.streamOne(ctx, streamURL); err != nil {
			if err == context.Canceled || err == context.DeadlineExceeded {
				return err
			}
			// Log and continue to next video on ffmpeg error
			idx++
			continue
		}
		idx++
	}
}

// streamOne runs ffmpeg to push a single URL to RTMP.
func (w *StreamWorker) streamOne(ctx context.Context, streamURL string) error {
	args := []string{"-hide_banner", "-loglevel", "warning"}
	if w.cfg.Streamer.Realtime {
		args = append(args, "-re")
	}
	args = append(args, "-i", streamURL, "-c", "copy", "-f", "flv", w.cfg.Owncast.RTMPIngestURL())

	cmd := exec.CommandContext(ctx, w.cfg.Streamer.FFmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(io.Discard, stdout); wg.Done() }()
	go func() { io.Copy(io.Discard, stderr); wg.Done() }()
	err = cmd.Wait()
	wg.Wait()
	return err
}
