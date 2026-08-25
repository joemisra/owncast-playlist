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
	CmdRewind
)

// StreamWorker streams videos from a playlist to Owncast via RTMP.
type StreamWorker struct {
	cfg      *config.Config
	registry *providers.Registry
	cmdCh    chan Command
	paused   atomic.Bool
	playing  atomic.Bool
	current  atomic.Value // stores string (current URL)
	phase    atomic.Value // idle, resolving, downloading, streaming, holding, retrying
	index    atomic.Int32

	playlistMu sync.RWMutex
	playlist   *playlist.Playlist

	subtitles atomic.Bool
	stopped   atomic.Bool
	skipped   atomic.Bool
	restart   atomic.Bool
	rewind    atomic.Bool
	jumpIndex atomic.Int32

	ffmpegMu     sync.Mutex
	ffmpegCancel context.CancelFunc
	ffmpegCmd    *exec.Cmd
}

// New creates a new stream worker.
func New(cfg *config.Config) *StreamWorker {
	reg := providers.NewRegistry()
	plexServers := make([]providers.PlexServer, 0, len(cfg.Plex.Servers))
	for _, s := range cfg.Plex.Servers {
		plexServers = append(plexServers, providers.PlexServer{Name: s.Name, BaseURL: s.BaseURL, Token: s.Token})
	}
	reg.Register(providers.NewPlex(plexServers))
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
	w.phase.Store("idle")
	w.jumpIndex.Store(-1)
	return w
}

func (w *StreamWorker) Send(cmd Command)       { w.cmdCh <- cmd }
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
func (w *StreamWorker) Phase() string {
	if value := w.phase.Load(); value != nil {
		return value.(string)
	}
	return "idle"
}

func (w *StreamWorker) SetYouTubeCookiesFile(path string) {
	if yt, ok := w.registry.Get("youtube").(*providers.YouTube); ok && yt != nil {
		yt.SetCookiesFile(path)
	}
}

// CurrentPlaylist returns a snapshot safe to read without holding the worker lock.
func (w *StreamWorker) CurrentPlaylist() *playlist.Playlist {
	w.playlistMu.RLock()
	defer w.playlistMu.RUnlock()
	if w.playlist == nil {
		return nil
	}
	cp := *w.playlist
	cp.Videos = append([]playlist.VideoEntry(nil), w.playlist.Videos...)
	return &cp
}

func (w *StreamWorker) SetPlaylist(pl *playlist.Playlist) {
	w.playlistMu.Lock()
	defer w.playlistMu.Unlock()
	w.playlist = pl
	w.Send(CmdRewind)
}

// JumpTo interrupts the current item and starts the requested playlist index.
func (w *StreamWorker) JumpTo(index int) {
	w.jumpIndex.Store(int32(index))
	w.skipped.Store(true)
	w.paused.Store(false)
	w.cancelFFmpeg()
}

// MoveVideo reorders an item and returns whether playback had to restart.
func (w *StreamWorker) MoveVideo(from, to int) (bool, error) {
	w.playlistMu.Lock()
	defer w.playlistMu.Unlock()
	if w.playlist == nil || from < 0 || from >= len(w.playlist.Videos) || to < 0 || to >= len(w.playlist.Videos) {
		return false, fmt.Errorf("invalid playlist index")
	}
	if from == to {
		return false, nil
	}
	item := w.playlist.Videos[from]
	copy(w.playlist.Videos[from:], w.playlist.Videos[from+1:])
	w.playlist.Videos = w.playlist.Videos[:len(w.playlist.Videos)-1]
	w.playlist.Videos = append(w.playlist.Videos, playlist.VideoEntry{})
	copy(w.playlist.Videos[to+1:], w.playlist.Videos[to:])
	w.playlist.Videos[to] = item
	current := int(w.index.Load())
	newCurrent := current
	if current == from {
		newCurrent = to
	} else if from < current && to >= current {
		newCurrent--
	} else if from > current && to <= current {
		newCurrent++
	}
	if newCurrent != current {
		w.index.Store(int32(newCurrent))
		go w.JumpTo(newCurrent)
		return true, nil
	}
	return false, nil
}

// RemoveVideo removes an item while keeping the playback cursor coherent.
func (w *StreamWorker) RemoveVideo(index int) error {
	w.playlistMu.Lock()
	if w.playlist == nil || index < 0 || index >= len(w.playlist.Videos) {
		w.playlistMu.Unlock()
		return fmt.Errorf("invalid playlist index")
	}
	current := int(w.index.Load())
	w.playlist.Videos = append(w.playlist.Videos[:index], w.playlist.Videos[index+1:]...)
	remaining := len(w.playlist.Videos)
	w.playlistMu.Unlock()
	if remaining == 0 {
		w.cancelFFmpeg()
		return nil
	}
	if index <= current {
		newCurrent := current
		if index < current {
			newCurrent--
		}
		if newCurrent >= remaining {
			newCurrent = remaining - 1
		}
		w.index.Store(int32(newCurrent))
		w.JumpTo(newCurrent)
	}
	return nil
}

// WithPlaylist runs fn with the live playlist pointer while holding the lock (for TUI edits).
func (w *StreamWorker) WithPlaylist(fn func(*playlist.Playlist)) {
	w.playlistMu.Lock()
	defer w.playlistMu.Unlock()
	fn(w.playlist)
}

// PlaylistSnapshot returns name, a copy of videos, and current index for UI display.
func (w *StreamWorker) PlaylistSnapshot() (name string, videos []playlist.VideoEntry, curIdx int) {
	w.playlistMu.RLock()
	defer w.playlistMu.RUnlock()
	curIdx = int(w.index.Load())
	if w.playlist != nil {
		name = w.playlist.Name
		videos = append([]playlist.VideoEntry(nil), w.playlist.Videos...)
	}
	return name, videos, curIdx
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
			case CmdRewind:
				w.rewind.Store(true)
				w.skipped.Store(true)
				w.cancelFFmpeg()
				log.Println("Rewind — restarting playlist from beginning")
			}
		}
	}
}

// StreamPlaylist streams all videos in the playlist to Owncast, using a blue holding pattern
// whenever the RTMP path would otherwise be idle (gaps, retries, empty playlist, end of list).
func (w *StreamWorker) StreamPlaylist(ctx context.Context, pl *playlist.Playlist) error {
	w.playlistMu.Lock()
	w.playlist = pl
	w.playlistMu.Unlock()

	w.playing.Store(true)
	w.phase.Store("starting")
	w.stopped.Store(false)
	w.skipped.Store(false)
	w.restart.Store(false)
	defer func() { w.playing.Store(false); w.phase.Store("idle") }()

	cmdCtx, cmdCancel := context.WithCancel(ctx)
	defer cmdCancel()
	go w.processCommands(cmdCtx)

	idx := 0
	w.index.Store(int32(idx))

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.stopped.Load() {
			return nil
		}
		if requested := int(w.jumpIndex.Swap(-1)); requested >= 0 {
			idx = requested
			w.skipped.Store(false)
			w.index.Store(int32(idx))
		}
		if w.rewind.CompareAndSwap(true, false) {
			idx = 0
			w.index.Store(0)
			log.Println("Loop rewound to index 0")
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
		}

		w.playlistMu.RLock()
		pl := w.playlist
		if pl == nil {
			w.playlistMu.RUnlock()
			if err := w.holdUntilPlaylist(ctx); err != nil {
				return err
			}
			continue
		}
		n := len(pl.Videos)
		if n == 0 {
			w.playlistMu.RUnlock()
			if err := w.holdUntilPlaylist(ctx); err != nil {
				return err
			}
			continue
		}

		if idx < 0 {
			idx = 0
		}
		if n > 0 && idx > n {
			idx = n - 1
			w.index.Store(int32(idx))
		}
		if idx >= n {
			if w.cfg.Streamer.LoopPlaylist {
				idx = 0
			} else {
				w.playlistMu.RUnlock()
				log.Println("Playlist at end — holding stream (blue) until more items are added")
				if err := w.holdUntilMoreContent(ctx, idx); err != nil {
					return err
				}
				continue
			}
		}

		entry := pl.Videos[idx]
		w.playlistMu.RUnlock()

		providerName := entry.Provider
		if providerName == "" {
			providerName = providers.InferProviderFromURL(entry.URL)
		}
		provider := w.registry.Get(providerName)
		if provider == nil {
			if providerName == "" {
				log.Printf("Could not infer provider from URL, skipping: %s", entry.URL)
			} else {
				log.Printf("Unknown provider %q, skipping", providerName)
			}
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

			log.Printf("[%d/%d] (attempt %d/%d) Streaming %s...", idx+1, n, attempt, w.cfg.Streamer.MaxRetries, entry.URL)
			w.phase.Store("resolving")

			var err error
			if provider.StreamViaPipe() {
				err = w.streamOneViaPipe(ctx, entry.URL, provider)
			} else {
				var streamURL string
				streamURL, err = provider.GetStreamURL(entry.URL)
				if err == nil {
					w.phase.Store("streaming")
					isLocal := provider.Name() == "local"
					err = w.streamWithRestart(ctx, streamURL, isLocal)
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
			w.phase.Store("retrying")
			log.Printf("Attempt %d failed: %v", attempt, err)
			if attempt < w.cfg.Streamer.MaxRetries {
				log.Printf("Waiting %ds before retry (holding stream)...", w.cfg.Streamer.DelayBetween)
				w.streamGap(ctx, time.Duration(w.cfg.Streamer.DelayBetween)*time.Second)
			}
		}

		if lastErr != nil {
			log.Printf("Skipping %s after %d attempts: %v", entry.URL, w.cfg.Streamer.MaxRetries, lastErr)
		}

		idx++

		w.playlistMu.RLock()
		nextN := 0
		if w.playlist != nil {
			nextN = len(w.playlist.Videos)
		}
		w.playlistMu.RUnlock()
		if idx < nextN && nextN > 0 {
			log.Printf("Gap before next video (%ds, holding stream)...", w.cfg.Streamer.DelayBetween)
			w.phase.Store("holding")
			w.streamGap(ctx, time.Duration(w.cfg.Streamer.DelayBetween)*time.Second)
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
		if w.stopped.Load() || w.skipped.Load() || w.rewind.Load() {
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
// While downloading, a blue holding pattern keeps the RTMP connection live.
func (w *StreamWorker) streamOneViaPipe(ctx context.Context, videoURL string, provider providers.VideoProvider) error {
	yt, ok := provider.(*providers.YouTube)
	if !ok {
		return fmt.Errorf("download-then-stream only supported for YouTube")
	}

	holdCtx, cancelHold := context.WithCancel(ctx)
	w.phase.Store("downloading")
	defer cancelHold()
	holdDone := make(chan struct{})
	go func() {
		defer close(holdDone)
		_ = w.runHoldingFFmpeg(holdCtx)
	}()

	tmpPath, err := w.downloadVideo(ctx, videoURL, yt)
	cancelHold()
	w.cancelFFmpeg()
	<-holdDone

	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	w.phase.Store("streaming")
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

	args := []string{"-f", "bv[ext=mp4]+ba[ext=m4a]/b",
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
		// Copy to temp so yt-dlp's --cookies read-write behavior can't clobber
		// the original. yt-dlp reads AND writes the same --cookies file.
		cookieTmp := tmpPath + ".cookies.txt"
		data, err := os.ReadFile(cookiesFile)
		if err != nil {
			return "", fmt.Errorf("read cookies: %w", err)
		}
		if err := os.WriteFile(cookieTmp, data, 0600); err != nil {
			return "", fmt.Errorf("write cookie tmp: %w", err)
		}
		defer os.Remove(cookieTmp)
		args = append(args, "--cookies", cookieTmp)
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

// needsTranscode checks if a file's video codec is not compatible with RTMP/FLV.
func (w *StreamWorker) needsTranscode(path string) bool {
	ffprobe := "ffprobe"
	if w.cfg.Streamer.FFmpegPath != "" {
		// Assume ffprobe is beside ffmpeg
		ffprobe = strings.TrimSuffix(w.cfg.Streamer.FFmpegPath, "ffmpeg") + "ffprobe"
	}
	cmd := exec.Command(ffprobe, "-v", "error",
		"-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	codec := strings.TrimSpace(string(out))
	return codec != "" && codec != "h264"
}

func (w *StreamWorker) buildFFmpegArgs(input string, burnSubs bool, loop bool, transcode bool) []string {
	args := []string{"-hide_banner", "-loglevel", "warning"}
	if loop {
		args = append(args, "-stream_loop", "-1")
	}
	if w.cfg.Streamer.Realtime {
		args = append(args, "-re")
	}
	args = append(args, "-i", input)

	if burnSubs || transcode {
		vf := ""
		if burnSubs {
			vf = fmt.Sprintf("subtitles=%s", ffmpegFilterEscape(input))
		}
		if vf != "" {
			args = append(args, "-vf", vf)
		}
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23")
	} else {
		args = append(args, "-c:v", "copy")
	}

	args = append(args, "-c:a", "aac", "-ar", "44100", "-f", "flv", w.cfg.Owncast.RTMPIngestURL())
	return args
}

// allLocal returns true if all video entries use the local provider.
func allLocal(videos []playlist.VideoEntry) bool {
	for _, v := range videos {
		if v.Provider != "local" && (v.Provider != "" || strings.HasPrefix(v.URL, "http")) {
			return false
		}
	}
	return len(videos) > 0
}

// streamConcatPlaylist streams all local videos in the current playlist as a single ffmpeg instance
// using the concat demuxer. This avoids RTMP reconnection between videos.
func (w *StreamWorker) streamConcatPlaylist(ctx context.Context, pl *playlist.Playlist, transcodeNeed bool) error {
	// Build concat file
	w.playlistMu.RLock()
	videos := make([]string, len(pl.Videos))
	for i, v := range pl.Videos {
		videos[i] = v.URL
	}
	w.playlistMu.RUnlock()

	var buf strings.Builder
	for _, path := range videos {
		// Escape single quotes for ffmpeg concat
		escaped := strings.ReplaceAll(path, "'", "'\\\\''")
		buf.WriteString(`file '`)
		buf.WriteString(escaped)
		buf.WriteString("'\n")
	}

	tempDir := w.cfg.Streamer.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	concatFile, err := os.CreateTemp(tempDir, "playlist-concat-*.txt")
	if err != nil {
		return fmt.Errorf("create concat: %w", err)
	}
	concatPath := concatFile.Name()
	defer os.Remove(concatPath)
	if _, err := concatFile.WriteString(buf.String()); err != nil {
		concatFile.Close()
		return fmt.Errorf("write concat: %w", err)
	}
	if err := concatFile.Close(); err != nil {
		return fmt.Errorf("close concat: %w", err)
	}

	log.Printf("[concat] Streaming %d videos via concat demuxer", len(videos))

	args := []string{"-hide_banner", "-loglevel", "warning"}
	if w.cfg.Streamer.Realtime {
		args = append(args, "-re")
	}
	args = append(args, "-f", "concat", "-safe", "0", "-i", concatPath)

	if transcodeNeed {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23")
	} else {
		args = append(args, "-c:v", "copy")
	}
	args = append(args, "-c:a", "aac", "-ar", "44100", "-f", "flv", w.cfg.Owncast.RTMPIngestURL())

	return w.runConcatFFmpeg(ctx, args)
}

// runConcatFFmpeg runs a single ffmpeg process with the given args and handles commands.
func (w *StreamWorker) runConcatFFmpeg(ctx context.Context, args []string) error {
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

	cmd := exec.CommandContext(ffCtx, w.cfg.Streamer.FFmpegPath, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	// Read stderr for logging
	sc := bufio.NewScanner(stderr)
	go func() {
		for sc.Scan() {
			log.Printf("[ffmpeg] %s", sc.Text())
		}
	}()

	// Wait for ffmpeg to finish or be killed
	err = cmd.Wait()

	// Check if we were stopped/skipped/rewound (not an error)
	if w.stopped.Load() || w.skipped.Load() || w.rewind.Load() {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
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
	// Use -stream_loop -1 only for single-file local playlists (seamless loop)
	loop := false
	transcode := false
	if localFile {
		w.playlistMu.RLock()
		if w.playlist != nil && len(w.playlist.Videos) == 1 {
			loop = true
		}
		w.playlistMu.RUnlock()
		transcode = w.needsTranscode(streamURL)
	}
	args := w.buildFFmpegArgs(streamURL, burnSubs, loop, transcode)

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
