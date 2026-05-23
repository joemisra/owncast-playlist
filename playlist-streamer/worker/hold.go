package worker

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"time"
)

// buildHoldingArgs returns ffmpeg args for an infinite blue video + silent audio to RTMP.
func (w *StreamWorker) buildHoldingArgs() []string {
	width := w.cfg.Streamer.HoldWidth
	height := w.cfg.Streamer.HoldHeight
	fps := w.cfg.Streamer.HoldFPS
	color := fmt.Sprintf("color=c=blue:s=%dx%d:r=%d", width, height, fps)
	gop := fps * 2
	if gop < 60 {
		gop = 60
	}
	return []string{
		"-hide_banner", "-loglevel", "warning",
		"-re",
		"-f", "lavfi", "-i", color,
		"-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=44100",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage", "-pix_fmt", "yuv420p",
		"-g", fmt.Sprintf("%d", gop),
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
		"-f", "flv", w.cfg.Owncast.RTMPIngestURL(),
	}
}

// runHoldingFFmpeg pushes a blue holding pattern to RTMP until ctx is cancelled or the process exits.
func (w *StreamWorker) runHoldingFFmpeg(ctx context.Context) error {
	ffCtx, ffCancel := context.WithCancel(ctx)
	defer ffCancel()

	w.ffmpegMu.Lock()
	w.ffmpegCancel = ffCancel
	w.ffmpegMu.Unlock()
	defer func() {
		w.ffmpegMu.Lock()
		w.ffmpegCancel = nil
		w.ffmpegMu.Unlock()
	}()

	args := w.buildHoldingArgs()
	cmd := exec.CommandContext(ffCtx, w.cfg.Streamer.FFmpegPath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg holding start: %w", err)
	}

	w.ffmpegMu.Lock()
	w.ffmpegCmd = cmd
	w.ffmpegMu.Unlock()
	defer func() {
		w.ffmpegMu.Lock()
		w.ffmpegCmd = nil
		w.ffmpegMu.Unlock()
	}()

	err := cmd.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Sub-context cancel (e.g. gap timeout) while parent ctx is still active — not a failure.
		if ffCtx.Err() != nil {
			return nil
		}
		return fmt.Errorf("ffmpeg holding: %w", err)
	}
	return nil
}

// streamGap shows the holding pattern for duration d (or until ctx cancel / stop / skip).
func (w *StreamWorker) streamGap(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	gapCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return w.runHoldingFFmpeg(gapCtx)
}

// holdUntilPlaylist runs the holding pattern until the playlist has at least one video.
func (w *StreamWorker) holdUntilPlaylist(ctx context.Context) error {
	logged := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.stopped.Load() {
			return nil
		}
		w.playlistMu.RLock()
		n := 0
		if w.playlist != nil {
			n = len(w.playlist.Videos)
		}
		w.playlistMu.RUnlock()
		if n > 0 {
			return nil
		}
		if !logged {
			log.Println("Playlist empty — holding stream (blue) until items are added")
			logged = true
		}
		if err := w.runHoldingFFmpeg(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if w.stopped.Load() {
				return nil
			}
			log.Printf("holding pattern: %v (retrying)", err)
			time.Sleep(time.Second)
		}
	}
}

// holdUntilMoreContent runs the holding pattern until len(videos) > idx (e.g. user appended after end).
func (w *StreamWorker) holdUntilMoreContent(ctx context.Context, idx int) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.stopped.Load() {
			return nil
		}
		w.playlistMu.RLock()
		n := 0
		if w.playlist != nil {
			n = len(w.playlist.Videos)
		}
		w.playlistMu.RUnlock()
		if n > idx {
			return nil
		}
		if err := w.runHoldingFFmpeg(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if w.stopped.Load() {
				return nil
			}
			log.Printf("holding pattern: %v (retrying)", err)
			time.Sleep(time.Second)
		}
	}
}
