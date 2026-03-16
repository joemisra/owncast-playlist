package worker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/providers"
)

type Command int

const (
	CmdPlay Command = iota
	CmdPause
	CmdStop
	CmdSkip
	CmdSubsOn
	CmdSubsOff
)

// StreamWorker streams videos from a playlist to Owncast via RTMP.
type StreamWorker struct {
	cfg      *config.Config
	registry *providers.Registry
	cmdCh    chan Command
	paused   atomic.Bool
	playing  atomic.Bool
	current  atomic.Value // stores string (current URL)
	index    atomic.Int32
	playlist atomic.Value // stores *playlist.Playlist

	subtitles atomic.Bool
	stopped   atomic.Bool
	skipped   atomic.Bool
	restart   atomic.Bool

	ffmpegMu     sync.Mutex
	ffmpegCancel context.CancelFunc
	ffmpegCmd    *exec.Cmd
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
	if rd, ok := reg.Get("realdebrid").(*providers.RealDebrid); ok && rd != nil {
		if cfg.Streamer.RealDebridToken != "" {
			rd.SetAPIToken(cfg.Streamer.RealDebridToken)
		}
	}
	w := &StreamWorker{
		cfg:      cfg,
		registry: reg,
		cmdCh:    make(chan Command, 8),
	}
	w.subtitles.Store(cfg.Streamer.Subtitles)
	return w
}

func (w *StreamWorker) Send(cmd Command)      { w.cmdCh <- cmd }
func (w *StreamWorker) IsPaused() bool         { return w.paused.Load() }
func (w *StreamWorker) IsPlaying() bool        { return w.playing.Load() }
func (w *StreamWorker) SubtitlesEnabled() bool { return w.subtitles.Load() }
func (w *StreamWorker) CurrentURL() string {
	if v := w.current.Load(); v != nil {
		return v.(string)
	}
	return ""
}
func (w *StreamWorker) CurrentIndex() int { return int(w.index.Load()) }
func (w *StreamWorker) CurrentPlaylist() *playlist.Playlist {
	if v := w.playlist.Load(); v != nil {
		return v.(*playlist.Playlist)
	}
	return nil
}

func (w *StreamWorker) SetPlaylist(pl *playlist.Playlist) {
	w.playlist.Store(pl)
}

func (w *StreamWorker) cancelFFmpeg() {
	w.ffmpegMu.Lock()
	if w.ffmpegCancel != nil {
		w.ffmpegCancel()
	}
	if w.ffmpegCmd != nil && w.ffmpegCmd.Process != nil {
		w.ffmpegCmd.Process.Kill()
	}
	w.ffmpegMu.Unlock()
}

// processCommands runs in a goroutine for the lifetime of StreamPlaylist,
// handling commands even while ffmpeg is actively streaming.
func (w *StreamWorker) processCommands(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-w.cmdCh:
			switch cmd {
			case CmdPause:
				w.paused.Store(true)
				log.Println("Paused. Send 'play' to resume.")
			case CmdPlay:
				w.paused.Store(false)
				log.Println("Resumed.")
			case CmdStop:
				w.stopped.Store(true)
				w.paused.Store(false)
				w.cancelFFmpeg()
			case CmdSkip:
				w.skipped.Store(true)
				w.paused.Store(false)
				w.cancelFFmpeg()
			case CmdSubsOn:
				w.subtitles.Store(true)
				w.restart.Store(true)
				log.Println("Subtitles enabled")
				w.cancelFFmpeg()
			case CmdSubsOff:
				w.subtitles.Store(false)
				w.restart.Store(true)
				log.Println("Subtitles disabled")
				w.cancelFFmpeg()
			}
		}
	}
}

// StreamPlaylist streams all videos in the playlist to Owncast.
func (w *StreamWorker) StreamPlaylist(ctx context.Context, pl *playlist.Playlist) error {
	w.playlist.Store(pl)
	w.playing.Store(true)
	w.stopped.Store(false)
	w.skipped.Store(false)
	w.restart.Store(false)
	defer w.playing.Store(false)

	cmdCtx, cmdCancel := context.WithCancel(ctx)
	defer cmdCancel()
	go w.processCommands(cmdCtx)

	idx := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.stopped.Load() {
			return nil
		}

		for w.paused.Load() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if w.stopped.Load() {
				return nil
			}
			if w.skipped.Load() {
				w.skipped.Store(false)
				idx++
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		if w.skipped.CompareAndSwap(true, false) {
			idx++
			continue
		}

		currentPl := w.CurrentPlaylist()
		if currentPl == nil {
			return nil
		}

		if idx >= len(currentPl.Videos) {
			if !w.cfg.Streamer.LoopPlaylist {
				log.Println("Playlist complete.")
				return nil
			}
			idx = 0
		}

		entry := currentPl.Videos[idx]
		provider := w.registry.Get(entry.Provider)
		if provider == nil {
			log.Printf("Unknown provider %q, skipping", entry.Provider)
			idx++
			continue
		}

		w.index.Store(int32(idx))
		w.current.Store(entry.URL)

		var lastErr error
		for attempt := 1; attempt <= w.cfg.Streamer.MaxRetries; attempt++ {
			if w.stopped.Load() {
				return nil
			}
			if w.skipped.CompareAndSwap(true, false) {
				lastErr = nil
				break
			}

			log.Printf("[%d/%d] (attempt %d/%d) Streaming %s...", idx+1, len(currentPl.Videos), attempt, w.cfg.Streamer.MaxRetries, entry.URL)

			var err error
			if provider.StreamViaPipe() {
				err = w.streamOneViaPipe(ctx, entry.URL, provider)
			} else {
				var streamURL string
				streamURL, err = provider.GetStreamURL(entry.URL)
				if err == nil {
					err = w.streamWithRestart(ctx, streamURL, false)
				}
			}

			if err == nil {
				lastErr = nil
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if w.stopped.Load() {
				return nil
			}
			if w.skipped.CompareAndSwap(true, false) {
				lastErr = nil
				break
			}

			lastErr = err
			log.Printf("Attempt %d failed: %v", attempt, err)
			if attempt < w.cfg.Streamer.MaxRetries {
				log.Printf("Waiting %ds before retry...", w.cfg.Streamer.DelayBetween)
				w.waitInterruptible(ctx, time.Duration(w.cfg.Streamer.DelayBetween)*time.Second)
			}
		}

		if lastErr != nil {
			log.Printf("Skipping %s after %d attempts: %v", entry.URL, w.cfg.Streamer.MaxRetries, lastErr)
		}

		idx++

		if idx < len(currentPl.Videos) {
			log.Printf("Waiting %ds before next video...", w.cfg.Streamer.DelayBetween)
			w.waitInterruptible(ctx, time.Duration(w.cfg.Streamer.DelayBetween)*time.Second)
		}
	}
}

// waitInterruptible sleeps for d but returns early on context cancel, stop, or skip.
func (w *StreamWorker) waitInterruptible(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.stopped.Load() || w.skipped.Load() {
				return
			}
		}
	}
}

// streamWithRestart runs ffmpeg and automatically restarts if a settings change
// (like subtitle toggle) is requested mid-stream.
func (w *StreamWorker) streamWithRestart(ctx context.Context, input string, localFile bool) error {
	w.restart.Store(false)
	for {
		err := w.streamOne(ctx, input, localFile)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.stopped.Load() || w.skipped.Load() {
			return err
		}
		if w.restart.CompareAndSwap(true, false) {
			log.Println("Restarting stream with updated settings...")
			continue
		}
		return err
	}
}

// streamOneViaPipe downloads the video with yt-dlp first, then streams the file to RTMP.
func (w *StreamWorker) streamOneViaPipe(ctx context.Context, videoURL string, provider providers.VideoProvider) error {
	yt, ok := provider.(*providers.YouTube)
	if !ok {
		return fmt.Errorf("download-then-stream only supported for YouTube")
	}

	tmpPath, err := w.downloadVideo(ctx, videoURL, yt)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	return w.streamWithRestart(ctx, tmpPath, true)
}

func (w *StreamWorker) downloadVideo(ctx context.Context, videoURL string, yt *providers.YouTube) (string, error) {
	tempDir := w.cfg.Streamer.TempDir
	if tempDir == "" {
		tempDir = "data/tmp"
	}
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	tmpFile, err := os.CreateTemp(tempDir, "yt-*.mkv")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(tmpPath)

	args := []string{"-f", "bestvideo[vcodec^=avc1]+bestaudio[acodec^=mp4a]/bestvideo[vcodec^=avc1]+bestaudio/bestvideo+bestaudio/best",
		"-o", tmpPath, "--merge-output-format", "mkv", "--no-part",
		"--js-runtimes", "node", "--remote-components", "ejs:github"}

	subLang := w.cfg.Streamer.SubtitleLang
	if subLang == "" {
		subLang = "en"
	}
	args = append(args, "--write-sub", "--write-auto-sub", "--sub-lang", subLang, "--embed-subs")

	cookiesFile := yt.GetCookiesFile()
	if cookiesFile != "" {
		if _, err := os.Stat(cookiesFile); err != nil {
			return "", fmt.Errorf("cookies file not found: %s", cookiesFile)
		}
		args = append(args, "--cookies", cookiesFile)
	}
	if yt.GetCookiesFromBrowser() != "" {
		args = append(args, "--cookies-from-browser", yt.GetCookiesFromBrowser())
	}
	args = append(args, videoURL)

	log.Printf("Downloading %s (with subtitles)...", videoURL)
	dlCmd := exec.CommandContext(ctx, w.cfg.Streamer.YtdlpPath, args...)
	if out, err := dlCmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("yt-dlp download: %w (output: %s)", err, string(out))
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return "", fmt.Errorf("download completed but file missing: %w", err)
	}
	if fi.Size() < 100000 {
		os.Remove(tmpPath)
		return "", fmt.Errorf("downloaded file too small (%d bytes) - likely failed or thumbnail only", fi.Size())
	}
	log.Printf("Download complete (%d MB), streaming to RTMP...", fi.Size()/(1024*1024))

	return tmpPath, nil
}

// ffmpegFilterEscape escapes a file path for use inside an ffmpeg filter graph.
func ffmpegFilterEscape(path string) string {
	path = strings.ReplaceAll(path, `\`, `\\`)
	path = strings.ReplaceAll(path, `:`, `\:`)
	path = strings.ReplaceAll(path, `'`, `\'`)
	path = strings.ReplaceAll(path, `[`, `\[`)
	path = strings.ReplaceAll(path, `]`, `\]`)
	return path
}

func (w *StreamWorker) buildFFmpegArgs(input string, burnSubs bool) []string {
	args := []string{"-hide_banner", "-loglevel", "warning"}
	if w.cfg.Streamer.Realtime {
		args = append(args, "-re")
	}
	args = append(args, "-i", input)

	if burnSubs {
		vf := fmt.Sprintf("subtitles=%s", ffmpegFilterEscape(input))
		args = append(args, "-vf", vf,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23")
	} else {
		args = append(args, "-c:v", "copy")
	}

	args = append(args, "-c:a", "aac", "-ar", "44100", "-f", "flv", w.cfg.Owncast.RTMPIngestURL())
	return args
}

// streamOne runs ffmpeg to push a single URL/file to RTMP.
func (w *StreamWorker) streamOne(ctx context.Context, streamURL string, localFile bool) error {
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

	burnSubs := w.subtitles.Load() && localFile
	args := w.buildFFmpegArgs(streamURL, burnSubs)

	if burnSubs {
		log.Println("Streaming with burned-in subtitles (transcoding)")
	}

	cmd := exec.CommandContext(ffCtx, w.cfg.Streamer.FFmpegPath, args...)
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

	w.ffmpegMu.Lock()
	w.ffmpegCmd = cmd
	w.ffmpegMu.Unlock()
	defer func() {
		w.ffmpegMu.Lock()
		w.ffmpegCmd = nil
		w.ffmpegMu.Unlock()
	}()

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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}
