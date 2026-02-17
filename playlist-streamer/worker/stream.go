package worker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
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
		if cfg.Streamer.CookiesFile != "" {
			yt.SetCookiesFile(cfg.Streamer.CookiesFile)
		}
		if cfg.Streamer.CookiesFromBrowser != "" {
			yt.SetCookiesFromBrowser(cfg.Streamer.CookiesFromBrowser)
		}
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
		log.Printf("[%d/%d] Streaming %s...", idx+1, len(pl.Videos), entry.URL)
		log.Printf("Pushing to RTMP %s", w.cfg.Owncast.RTMPURL)
		var err error
		if provider.StreamViaPipe() {
			err = w.streamOneViaPipe(ctx, entry.URL, provider)
		} else {
			var streamURL string
			streamURL, err = provider.GetStreamURL(entry.URL)
			if err == nil {
				err = w.streamOne(ctx, streamURL)
			}
		}
		if err != nil {
			if err == context.Canceled || err == context.DeadlineExceeded {
				return err
			}
			log.Printf("Stream error: %v, skipping to next", err)
			idx++
			continue
		}
		idx++
	}
}

// streamOneViaPipe downloads the video with yt-dlp first, then streams the file
// to RTMP. This avoids pipe timing issues and works reliably with YouTube cookies.
func (w *StreamWorker) streamOneViaPipe(ctx context.Context, videoURL string, provider providers.VideoProvider) error {
	yt, ok := provider.(*providers.YouTube)
	if !ok {
		return fmt.Errorf("download-then-stream only supported for YouTube")
	}
	tempDir := w.cfg.Streamer.TempDir
	if tempDir == "" {
		tempDir = "data/tmp"
	}
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	tmpFile, err := os.CreateTemp(tempDir, "yt-*.mkv")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(tmpPath) // empty file; yt-dlp will create it
	defer os.Remove(tmpPath)

	// Download with yt-dlp. MKV container avoids moov-atom issues; bestvideo+bestaudio
	// forces a proper merge. --no-part ensures we don't read incomplete files.
	// --js-runtimes node + --remote-components ejs:github: required for YouTube's n-challenge
	// (install Node: sudo apt install nodejs).
	// Prefer H.264 video (avc1) so ffmpeg can copy it into FLV/RTMP without transcoding.
	// VP9/AV1 can't go into FLV and would need expensive re-encoding.
	args := []string{"-f", "bestvideo[vcodec^=avc1]+bestaudio[acodec^=mp4a]/bestvideo[vcodec^=avc1]+bestaudio/bestvideo+bestaudio/best",
		"-o", tmpPath, "--merge-output-format", "mkv", "--no-part",
		"--js-runtimes", "node", "--remote-components", "ejs:github"}
	cookiesFile := yt.GetCookiesFile()
	if cookiesFile != "" {
		if _, err := os.Stat(cookiesFile); err != nil {
			return fmt.Errorf("cookies file not found: %s", cookiesFile)
		}
		args = append(args, "--cookies", cookiesFile)
		log.Printf("Using cookies from %s", cookiesFile)
	} else {
		log.Printf("Warning: no cookies_file set - YouTube may block with 'Sign in to confirm you're not a bot'")
	}
	if yt.GetCookiesFromBrowser() != "" {
		args = append(args, "--cookies-from-browser", yt.GetCookiesFromBrowser())
	}
	args = append(args, videoURL)

	log.Printf("Downloading %s...", videoURL)
	dlCmd := exec.CommandContext(ctx, w.cfg.Streamer.YtdlpPath, args...)
	if out, err := dlCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("yt-dlp download: %w (output: %s)", err, string(out))
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return fmt.Errorf("download completed but file missing: %w", err)
	}
	if fi.Size() < 100000 {
		return fmt.Errorf("downloaded file too small (%d bytes) - likely failed or thumbnail only", fi.Size())
	}
	log.Printf("Download complete (%d MB), streaming to RTMP...", fi.Size()/(1024*1024))

	return w.streamOne(ctx, tmpPath)
}

// streamOne runs ffmpeg to push a single URL to RTMP.
func (w *StreamWorker) streamOne(ctx context.Context, streamURL string) error {
	args := []string{"-hide_banner", "-loglevel", "warning"}
	if w.cfg.Streamer.Realtime {
		args = append(args, "-re")
	}
	// FLV/RTMP needs AAC audio (no Opus); resample to 44100 if needed
	args = append(args, "-i", streamURL, "-c:v", "copy", "-c:a", "aac", "-ar", "44100", "-f", "flv", w.cfg.Owncast.RTMPIngestURL())

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
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(io.Discard, stdout); wg.Done() }()
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("[ffmpeg] %s", sc.Text())
		}
		wg.Done()
	}()
	err = cmd.Wait()
	wg.Wait()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}
