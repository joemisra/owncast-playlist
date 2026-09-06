package worker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
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
	cfg        *config.Config
	registry   *providers.Registry
	plexCache  *plexCache
	cmdCh      chan Command
	paused     atomic.Bool
	playing    atomic.Bool
	current    atomic.Value // stores string (current URL)
	phase      atomic.Value // idle, resolving, downloading, streaming, holding, retrying
	index      atomic.Int32
	cacheBytes atomic.Int64
	cacheTotal atomic.Int64
	media      mediaStatusStore

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
	runMu        sync.Mutex
	runCancel    context.CancelFunc
}

// New creates a new stream worker.
func New(cfg *config.Config) *StreamWorker {
	reg := providers.NewRegistry()
	plexServers := make([]providers.PlexServer, 0, len(cfg.Plex.Servers))
	for _, s := range cfg.Plex.Servers {
		plexServers = append(plexServers, providers.PlexServer{Name: s.Name, BaseURL: s.BaseURL, Token: s.Token})
	}
	reg.Register(providers.NewPlex(plexServers))
	smbShares := make([]providers.SMBShare, 0, len(cfg.SMB.Shares))
	for _, share := range cfg.SMB.Shares {
		smbShares = append(smbShares, providers.SMBShare{Name: share.Name, Path: share.Path})
	}
	reg.Register(providers.NewSMB(smbShares))
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
	if cfg.Streamer.PlexCacheEnabled {
		w.plexCache = newPlexCache(cfg.Streamer)
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
func (w *StreamWorker) CacheProgress() (int64, int64) {
	return w.cacheBytes.Load(), w.cacheTotal.Load()
}
func (w *StreamWorker) MediaItemStatuses() map[string]MediaItemStatus {
	return w.media.snapshot()
}
func (w *StreamWorker) CacheUploadStatus(cacheKey string) MediaItemStatus {
	if w.plexCache == nil {
		return MediaItemStatus{State: "failed", Detail: "Remote caching is disabled", UpdatedAt: time.Now()}
	}
	path, hit := w.plexCache.cachedPath(cacheKey, cacheKey)
	if hit {
		info, _ := os.Stat(path)
		return MediaItemStatus{State: "cached", Detail: "Ready on Couch", Bytes: info.Size(), Total: info.Size(), UpdatedAt: info.ModTime()}
	}
	if info, err := os.Stat(path + ".incoming"); err == nil && info.Mode().IsRegular() {
		return MediaItemStatus{State: "caching", Detail: "Uploading to Couch", Bytes: info.Size(), UpdatedAt: info.ModTime()}
	}
	if status, ok := w.media.snapshot()[cacheKey]; ok {
		return status
	}
	return MediaItemStatus{State: "waiting", Detail: "Not cached yet", UpdatedAt: time.Now()}
}

func (w *StreamWorker) ReceiveCacheUpload(ctx context.Context, cacheKey string, source io.Reader, total int64) error {
	if w.plexCache == nil {
		return fmt.Errorf("remote caching is disabled")
	}
	w.media.set(cacheKey, "caching", "Uploading to Couch", 0, total)
	_, err := w.plexCache.receive(ctx, cacheKey, source, total, func(received, total int64) {
		w.media.set(cacheKey, "caching", "Uploading to Couch", received, total)
	})
	if err != nil {
		w.media.set(cacheKey, "failed", "Upload to Couch failed", 0, total)
		return err
	}
	w.media.set(cacheKey, "cached", "Ready on Couch", total, total)
	return nil
}
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

func (w *StreamWorker) cancelRun() {
	w.runMu.Lock()
	if w.runCancel != nil {
		w.runCancel()
	}
	w.runMu.Unlock()
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
				w.cancelRun()
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
	runCtx, runCancel := context.WithCancel(ctx)
	w.runMu.Lock()
	w.runCancel = runCancel
	w.runMu.Unlock()
	defer func() {
		runCancel()
		w.runMu.Lock()
		w.runCancel = nil
		w.runMu.Unlock()
	}()
	ctx = runCtx

	w.playlistMu.Lock()
	w.playlist = pl
	w.playlistMu.Unlock()

	w.playing.Store(true)
	w.phase.Store("starting")
	w.stopped.Store(false)
	w.skipped.Store(false)
	w.restart.Store(false)
	w.cacheBytes.Store(0)
	w.cacheTotal.Store(0)
	w.media.reset(pl, w.plexCache != nil)
	defer func() { w.playing.Store(false); w.phase.Store("idle") }()

	cmdCtx, cmdCancel := context.WithCancel(ctx)
	defer cmdCancel()
	go w.processCommands(cmdCtx)

	// Local, SMB, and Plex playlists can be normalized and published by one ffmpeg
	// process. Keeping that process alive prevents Owncast from seeing every
	// item boundary (and the holding screen) as a brand-new RTMP stream.
	if handled, err := w.streamContinuousPlaylist(ctx, pl); handled {
		return err
	}

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
			w.media.set(entry.URL, "failed", "Unknown media provider", 0, 0)
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
			w.media.set(entry.URL, "failed", "Playback failed after all retry attempts", 0, 0)
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

type concatInput struct {
	resolved string
	original string
	duration float64
	index    int
}

type smbCacheItem struct {
	cacheKey   string
	source     string
	sourceSize int64
	index      int
}

// streamContinuousPlaylist handles local and Plex playlists with a
// single normalized RTMP publisher. It returns handled=false for providers
// that need their existing per-item path, such as YouTube.
func (w *StreamWorker) streamContinuousPlaylist(ctx context.Context, pl *playlist.Playlist) (bool, error) {
	if pl == nil || len(pl.Videos) == 0 {
		return false, nil
	}

	inputs := make([]concatInput, 0, len(pl.Videos))
	protectedCachePaths := make(map[string]bool)
	pendingSMB := make([]smbCacheItem, 0)
	var cacheHoldCancel context.CancelFunc
	var cacheHoldDone chan struct{}
	startCacheHold := func() {
		if cacheHoldCancel != nil {
			return
		}
		holdCtx, cancel := context.WithCancel(ctx)
		cacheHoldCancel = cancel
		cacheHoldDone = make(chan struct{})
		go func() {
			defer close(cacheHoldDone)
			_ = w.runHoldingFFmpeg(holdCtx)
		}()
	}
	stopCacheHold := func() {
		if cacheHoldCancel == nil {
			return
		}
		cacheHoldCancel()
		<-cacheHoldDone
		cacheHoldCancel = nil
		cacheHoldDone = nil
	}
	defer stopCacheHold()

	for i, entry := range pl.Videos {
		providerName := entry.Provider
		if providerName == "" {
			providerName = providers.InferProviderFromURL(entry.URL)
		}
		provider := w.registry.Get(providerName)
		if provider == nil || provider.StreamViaPipe() || (provider.Name() != "local" && provider.Name() != "plex" && provider.Name() != "smb") {
			return false, nil
		}
		resolved, err := provider.GetStreamURL(entry.URL)
		if err != nil {
			w.media.set(entry.URL, "failed", "The source is unavailable", 0, 0)
			if (provider.Name() == "plex" && w.plexCache != nil) || provider.Name() == "smb" {
				log.Printf("[media-cache] Skipping unavailable item %d: %v", i+1, err)
				continue
			}
			log.Printf("[continuous] Could not prepare item %d; using per-item streaming: %v", i+1, err)
			return false, nil
		}
		if provider.Name() == "plex" && w.plexCache != nil {
			cachePath, hit := w.plexCache.cachedPath(entry.URL, resolved)
			if !hit {
				w.media.set(entry.URL, "caching", "Downloading to Couch", 0, 0)
				startCacheHold()
				w.current.Store(entry.URL)
				w.index.Store(int32(i))
				w.phase.Store("downloading")
				log.Printf("[media-cache] Caching Plex item %d of %d", i+1, len(pl.Videos))
			} else {
				w.media.set(entry.URL, "cached", "Ready on Couch", 0, 0)
				log.Printf("[media-cache] Using cached Plex item %d of %d", i+1, len(pl.Videos))
			}
			for {
				cachePath, err = w.plexCache.fetch(ctx, entry.URL, resolved, protectedCachePaths, func(received, total int64) {
					w.cacheBytes.Store(received)
					w.cacheTotal.Store(total)
					w.media.set(entry.URL, "caching", "Downloading to Couch", received, total)
				})
				if err == nil || !isTemporaryPlexCacheError(err) {
					break
				}
				log.Printf("[media-cache] Plex download interrupted; resuming item %d in 5 seconds: %v", i+1, err)
				select {
				case <-ctx.Done():
					err = ctx.Err()
				case <-time.After(5 * time.Second):
				}
				if ctx.Err() != nil {
					break
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					stopCacheHold()
					return true, ctx.Err()
				}
				log.Printf("[media-cache] Skipping Plex item %d after cache failure: %v", i+1, err)
				w.media.set(entry.URL, "failed", "Could not cache this Plex item", 0, 0)
				continue
			}
			w.media.set(entry.URL, "cached", "Ready on Couch", 0, 0)
			resolved = cachePath
		}
		durationSource := resolved
		if provider.Name() == "smb" && w.plexCache != nil {
			// A Trees-side helper can push this logical SMB item to Couch over
			// public HTTPS. Prefer that completed file without touching the
			// Tailscale SMB mount; mounted SMB copying remains the fallback.
			if cachePath, hit := w.plexCache.cachedPath(entry.URL, entry.URL); hit {
				w.media.set(entry.URL, "cached", "Ready on Couch", 0, 0)
				log.Printf("[media-cache] Using pushed Trees item %d of %d", i+1, len(pl.Videos))
				duration := w.mediaDuration(ctx, cachePath)
				if duration <= 0 {
					w.media.set(entry.URL, "failed", "The cached file could not be read", 0, 0)
					log.Printf("[media-cache] Skipping unreadable pushed item %d", i+1)
					continue
				}
				inputs = append(inputs, concatInput{resolved: cachePath, original: entry.URL, duration: duration, index: i})
				continue
			}
			sourceInfo, statErr := os.Stat(resolved)
			if statErr != nil || !sourceInfo.Mode().IsRegular() {
				w.media.set(entry.URL, "failed", "Trees file is unavailable", 0, 0)
				log.Printf("[media-cache] Skipping unreadable SMB item %d", i+1)
				continue
			}
			cachePath, hit := w.plexCache.cachedFilePath(entry.URL, resolved, sourceInfo.Size())
			protectedCachePaths[cachePath] = true
			protectedCachePaths[cachePath+".partial"] = true
			if hit {
				w.media.set(entry.URL, "cached", "Ready on Couch", 0, 0)
				log.Printf("[media-cache] Using cached SMB item %d of %d", i+1, len(pl.Videos))
				resolved = cachePath
				durationSource = cachePath
			} else if len(inputs) == 0 {
				w.media.set(entry.URL, "caching", "Transferring from Trees", 0, sourceInfo.Size())
				startCacheHold()
				w.current.Store(entry.URL)
				w.index.Store(int32(i))
				w.phase.Store("downloading")
				log.Printf("[media-cache] Caching first SMB item before playback")
				if err := w.cacheSMBWithRetry(ctx, entry.URL, resolved, protectedCachePaths, true); err != nil {
					if ctx.Err() != nil {
						stopCacheHold()
						return true, ctx.Err()
					}
					log.Printf("[media-cache] Skipping first SMB item after cache failure: %v", err)
					w.media.set(entry.URL, "failed", "Could not transfer this Trees file", 0, sourceInfo.Size())
					continue
				}
				resolved = cachePath
				durationSource = cachePath
			} else {
				w.media.set(entry.URL, "waiting", "Waiting to transfer", 0, sourceInfo.Size())
				pendingSMB = append(pendingSMB, smbCacheItem{
					cacheKey: entry.URL, source: resolved, sourceSize: sourceInfo.Size(), index: i,
				})
				resolved = cachePath
			}
		}
		duration := w.mediaDuration(ctx, durationSource)
		log.Printf("[continuous] Prepared item %d (%0.3fs)", i+1, duration)
		if duration <= 0 {
			if (provider.Name() == "plex" || provider.Name() == "smb") && w.plexCache != nil {
				w.media.set(entry.URL, "failed", "The cached file could not be read", 0, 0)
				log.Printf("[media-cache] Skipping unreadable cached item %d", i+1)
				continue
			}
			log.Printf("[continuous] Item %d is not readable; using per-item retry handling", i+1)
			return false, nil
		}
		inputs = append(inputs, concatInput{
			resolved: resolved,
			original: entry.URL,
			duration: duration,
			index:    i,
		})
	}
	stopCacheHold()
	if len(inputs) == 0 {
		return true, fmt.Errorf("no playable local or cached remote items")
	}

	prefetchCtx, cancelPrefetch := context.WithCancel(ctx)
	prefetchDone := make(chan struct{})
	if len(pendingSMB) > 0 {
		go func() {
			defer close(prefetchDone)
			for _, item := range pendingSMB {
				if prefetchCtx.Err() != nil {
					return
				}
				if _, hit := w.plexCache.cachedFilePath(item.cacheKey, item.source, item.sourceSize); hit {
					w.media.set(item.cacheKey, "cached", "Ready on Couch", 0, item.sourceSize)
					continue
				}
				w.media.set(item.cacheKey, "caching", "Transferring from Trees", 0, item.sourceSize)
				log.Printf("[media-cache] Prefetching SMB item %d of %d", item.index+1, len(pl.Videos))
				if err := w.cacheSMBWithRetry(prefetchCtx, item.cacheKey, item.source, protectedCachePaths, false); err != nil && prefetchCtx.Err() == nil {
					w.media.set(item.cacheKey, "failed", "Trees transfer failed", 0, item.sourceSize)
					log.Printf("[media-cache] SMB prefetch failed for item %d: %v", item.index+1, err)
				}
			}
			w.cacheBytes.Store(0)
			w.cacheTotal.Store(0)
		}()
	} else {
		close(prefetchDone)
	}
	defer func() {
		cancelPrefetch()
		select {
		case <-prefetchDone:
		case <-time.After(5 * time.Second):
			log.Printf("[media-cache] SMB prefetch is still stopping")
		}
	}()

	start := 0
	for {
		ordered := append([]concatInput(nil), inputs[start:]...)
		ordered = append(ordered, inputs[:start]...)
		w.index.Store(int32(ordered[0].index))
		w.current.Store(ordered[0].original)
		w.phase.Store("streaming")
		log.Printf("[continuous] Publishing %d items over one RTMP connection", len(ordered))

		loop := w.cfg.Streamer.LoopPlaylist || len(inputs) == 1
		err := w.streamConcatPlaylist(ctx, ordered, loop)
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if w.stopped.Load() {
			return true, nil
		}

		if requested := int(w.jumpIndex.Swap(-1)); requested >= 0 && requested < len(inputs) {
			start = requested
			w.skipped.Store(false)
			w.rewind.Store(false)
			continue
		}
		if w.rewind.CompareAndSwap(true, false) {
			start = 0
			w.skipped.Store(false)
			continue
		}
		if w.skipped.CompareAndSwap(true, false) {
			start = (int(w.index.Load()) + 1) % len(inputs)
			continue
		}
		return true, err
	}
}

func (w *StreamWorker) cacheSMBWithRetry(ctx context.Context, cacheKey, source string, protected map[string]bool, foreground bool) error {
	for {
		_, err := w.plexCache.fetchFile(ctx, cacheKey, source, protected, func(received, total int64) {
			w.cacheBytes.Store(received)
			w.cacheTotal.Store(total)
			w.media.set(cacheKey, "caching", "Transferring from Trees", received, total)
		})
		if err == nil {
			w.media.set(cacheKey, "cached", "Ready on Couch", 0, 0)
			if foreground {
				log.Printf("[media-cache] SMB item cached locally")
			} else {
				log.Printf("[media-cache] SMB prefetch complete")
			}
			return nil
		}
		if !isTemporaryPlexCacheError(err) || ctx.Err() != nil {
			return err
		}
		log.Printf("[media-cache] SMB copy interrupted; resuming in 5 seconds: %v", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (w *StreamWorker) mediaDuration(ctx context.Context, input string) float64 {
	ffprobe := "ffprobe"
	if w.cfg.Streamer.FFmpegPath != "" {
		ffprobe = strings.TrimSuffix(w.cfg.Streamer.FFmpegPath, "ffmpeg") + "ffprobe"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", input)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	duration, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return duration
}

func concatPosition(elapsed float64, inputs []concatInput) int {
	total := 0.0
	for _, input := range inputs {
		if input.duration <= 0 {
			return 0
		}
		total += input.duration
	}
	if total <= 0 {
		return 0
	}
	elapsed = elapsed - float64(int64(elapsed/total))*total
	for i, input := range inputs {
		if elapsed < input.duration {
			return i
		}
		elapsed -= input.duration
	}
	return len(inputs) - 1
}

// streamConcatPlaylist streams resolved local or Plex videos as a single
// normalized ffmpeg instance. This avoids RTMP reconnection between videos.
func (w *StreamWorker) streamConcatPlaylist(ctx context.Context, inputs []concatInput, loop bool) error {
	var buf strings.Builder
	for _, input := range inputs {
		// Escape single quotes for ffmpeg concat
		escaped := strings.ReplaceAll(input.resolved, "'", "'\\\\''")
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

	log.Printf("[concat] Streaming %d videos via concat demuxer", len(inputs))

	args := []string{"-hide_banner", "-loglevel", "warning", "-nostats", "-progress", "pipe:1"}
	if w.cfg.Streamer.Realtime {
		args = append(args, "-re")
	}
	if loop {
		args = append(args, "-stream_loop", "-1")
	}
	args = append(args, "-protocol_whitelist", "file,http,https,tcp,tls,crypto,data", "-f", "concat", "-safe", "0", "-i", concatPath)

	width, height, fps := w.cfg.Streamer.HoldWidth, w.cfg.Streamer.HoldHeight, w.cfg.Streamer.HoldFPS
	if width <= 0 {
		width = 1280
	}
	if height <= 0 {
		height = 720
	}
	if fps <= 0 {
		fps = 30
	}
	filter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%d,format=yuv420p", width, height, width, height, fps)
	args = append(args,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
		"-vf", filter,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-g", strconv.Itoa(fps*2), "-keyint_min", strconv.Itoa(fps*2), "-sc_threshold", "0",
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100", "-af", "aresample=async=1:first_pts=0",
		"-f", "flv", w.cfg.Owncast.RTMPIngestURL())

	return w.runConcatFFmpeg(ctx, args, inputs)
}

// runConcatFFmpeg runs a single ffmpeg process with the given args and handles commands.
func (w *StreamWorker) runConcatFFmpeg(ctx context.Context, args []string, inputs []concatInput) error {
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
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

	progress := bufio.NewScanner(stdout)
	go func() {
		lastPosition := -1
		for progress.Scan() {
			line := progress.Text()
			if !strings.HasPrefix(line, "out_time_us=") {
				continue
			}
			micros, err := strconv.ParseFloat(strings.TrimPrefix(line, "out_time_us="), 64)
			if err != nil {
				continue
			}
			position := concatPosition(micros/1_000_000, inputs)
			w.index.Store(int32(inputs[position].index))
			w.current.Store(inputs[position].original)
			if position != lastPosition {
				log.Printf("[continuous] Now playing item %d", inputs[position].index+1)
				lastPosition = position
			}
		}
	}()

	sc := bufio.NewScanner(stderr)
	go func() {
		for sc.Scan() {
			log.Printf("[ffmpeg] %s", w.redactSecrets(sc.Text()))
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

func (w *StreamWorker) redactSecrets(line string) string {
	secrets := []string{w.cfg.Owncast.StreamKey, w.cfg.Streamer.RealDebridToken}
	for _, server := range w.cfg.Plex.Servers {
		secrets = append(secrets, server.Token)
	}
	for _, secret := range secrets {
		if secret != "" {
			line = strings.ReplaceAll(line, secret, "[redacted]")
		}
	}
	return line
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
			log.Printf("[ffmpeg] %s", w.redactSecrets(sc.Text()))
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
