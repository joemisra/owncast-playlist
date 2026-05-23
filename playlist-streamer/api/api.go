package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/providers"
	"playlist-streamer/worker"

	"gopkg.in/yaml.v3"
)

// Server wraps an HTTP listener with a reference to the stream worker.
type Server struct {
	cfg            *config.Config
	worker         *worker.StreamWorker
	playlistDir    string
	mux            *http.ServeMux
	srv            *http.Server
	stopContinuous atomic.Bool
	schedRunner    scheduleRunner
}

// New creates a new API server wired to the given worker.
func New(cfg *config.Config, w *worker.StreamWorker, playlistDir string) *Server {
	s := &Server{
		cfg:         cfg,
		worker:      w,
		playlistDir: playlistDir,
		mux:         http.NewServeMux(),
	}
	s.register()
	return s
}

// ShouldContinue returns false when /api/control/stop has been called,
// telling the continuous loop to break instead of restarting the stream.
func (s *Server) ShouldContinue() bool {
	return !s.stopContinuous.Load()
}

// seenFiles tracks files we've already added to the playlist.
var seenFiles = make(map[string]bool)
var seenMu sync.RWMutex

// startFileWatcher periodically scans the videos directory and auto-adds new files.
func (s *Server) startFileWatcher() {
	// Seed with existing files so we don't re-add them
	entries, err := os.ReadDir(videosDir)
	if err == nil {
		seenMu.Lock()
		for _, e := range entries {
			if !e.IsDir() {
				seenFiles[e.Name()] = true
			}
		}
		seenMu.Unlock()
	}

	go func() {
		for {
			time.Sleep(15 * time.Second)

			entries, err := os.ReadDir(videosDir)
			if err != nil {
				continue
			}

			var newFiles []string
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				seenMu.RLock()
				known := seenFiles[e.Name()]
				seenMu.RUnlock()
				if known {
					continue
				}
				// Skip non-video extensions
				name := strings.ToLower(e.Name())
				if !strings.HasSuffix(name, ".mp4") && !strings.HasSuffix(name, ".mkv") &&
					!strings.HasSuffix(name, ".mov") && !strings.HasSuffix(name, ".avi") &&
					!strings.HasSuffix(name, ".webm") && !strings.HasSuffix(name, ".flv") {
					seenMu.Lock()
					seenFiles[e.Name()] = true
					seenMu.Unlock()
					continue
				}
				newFiles = append(newFiles, e.Name())
			}

			if len(newFiles) == 0 {
				continue
			}

			for _, name := range newFiles {
				path := videosDir + "/" + name
				s.worker.WithPlaylist(func(pl *playlist.Playlist) {
					if pl != nil {
						pl.Videos = append(pl.Videos, playlist.VideoEntry{
							URL:      path,
							Provider: "local",
						})
						log.Printf("[watcher] Auto-added %s to playlist", name)
					}
				})
				seenMu.Lock()
				seenFiles[name] = true
				seenMu.Unlock()
			}

			s.worker.Send(worker.CmdRewind)
		}
	}()
}

// Listen starts the HTTP server on addr and returns when it exits.
func (s *Server) Listen(addr string) error {
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.dashboard(s.withCORS(s.mux)),
	}
	log.Printf("[api] Listening on %s", addr)
	log.Printf("[api] Dashboard at http://localhost%s/", addr)
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) register() {
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/control/pause", s.handlePause)
	s.mux.HandleFunc("/api/control/play", s.handlePlay)
	s.mux.HandleFunc("/api/control/skip", s.handleSkip)
	s.mux.HandleFunc("/api/control/stop", s.handleStop)
	s.mux.HandleFunc("/api/control/subs", s.handleSubs)
	s.mux.HandleFunc("/api/playlist", s.handlePlaylist)
	s.mux.HandleFunc("/api/playlist/add", s.handlePlaylistAdd)
	s.mux.HandleFunc("/api/playlist/video", s.handleAddVideo)
	s.mux.HandleFunc("/api/playlist/remove", s.handleRemoveVideo)
	s.mux.HandleFunc("/api/playlist/load", s.handleLoadPlaylist)
	s.mux.HandleFunc("/api/playlist/save", s.handleSavePlaylist)
	s.mux.HandleFunc("/api/upload", s.handleUploadVideo)
	s.mux.HandleFunc("/api/videos", s.handleListVideos)
	s.mux.HandleFunc("/api/admin/cookies", s.handleUploadCookies)
	s.mux.HandleFunc("/api/schedule", s.handleSchedule)
	s.mux.HandleFunc("/api/schedule/add", s.handleScheduleAdd)
	s.mux.HandleFunc("/api/schedule/remove", s.handleScheduleRemove)
	s.mux.HandleFunc("/api/schedule/save", s.handleScheduleSave)
	s.mux.HandleFunc("/api/schedule/run", s.handleScheduleRun)
	s.mux.HandleFunc("/api/schedule/stop", s.handleScheduleStop)
	s.mux.HandleFunc("/api/schedule/status", s.handleScheduleStatus)
}

// ── CORS ────────────────────────────────────────────────────────────

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── helpers ─────────────────────────────────────────────────────────

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ── Status ──────────────────────────────────────────────────────────

type statusResp struct {
	Playing      bool   `json:"playing"`
	Paused       bool   `json:"paused"`
	Subtitles    bool   `json:"subtitles"`
	CurrentURL   string `json:"currentUrl"`
	CurrentIndex int    `json:"currentIndex"`
	TotalVideos  int    `json:"totalVideos"`
	PlaylistName string `json:"playlistName"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "GET required")
		return
	}
	name, videos, curIdx := s.worker.PlaylistSnapshot()
	resp := statusResp{
		Playing:      s.worker.IsPlaying(),
		Paused:       s.worker.IsPaused(),
		Subtitles:    s.worker.SubtitlesEnabled(),
		CurrentURL:   s.worker.CurrentURL(),
		CurrentIndex: curIdx,
		TotalVideos:  len(videos),
		PlaylistName: name,
	}
	s.writeJSON(w, resp)
}

// ── Control commands ────────────────────────────────────────────────

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	s.worker.Send(worker.CmdPause)
	s.writeJSON(w, map[string]string{"status": "paused"})
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	s.worker.Send(worker.CmdPlay)
	s.writeJSON(w, map[string]string{"status": "playing"})
}

func (s *Server) handleSkip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	s.worker.Send(worker.CmdSkip)
	s.writeJSON(w, map[string]string{"status": "skipping"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	s.stopContinuous.Store(true)
	s.worker.Send(worker.CmdStop)
	log.Printf("[api] Stream stop requested — continuous loop will not restart")
	s.writeJSON(w, map[string]string{"status": "stopped"})
}

func (s *Server) handleSubs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, "invalid body")
		return
	}
	if body.Enabled {
		s.worker.Send(worker.CmdSubsOn)
	} else {
		s.worker.Send(worker.CmdSubsOff)
	}
	s.writeJSON(w, map[string]bool{"subtitles": body.Enabled})
}

// ── Playlist ────────────────────────────────────────────────────────

type playlistResp struct {
	Name         string                `json:"name"`
	Videos       []playlist.VideoEntry `json:"videos"`
	CurrentIndex int                   `json:"currentIndex"`
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		name, videos, curIdx := s.worker.PlaylistSnapshot()
		s.writeJSON(w, playlistResp{
			Name:         name,
			Videos:       videos,
			CurrentIndex: curIdx,
		})

	case http.MethodPost:
		var body struct {
			Name   string                `json:"name"`
			Videos []playlist.VideoEntry `json:"videos"`
		}
		if err := s.decodeBody(r, &body); err != nil {
			s.writeError(w, 400, fmt.Sprintf("invalid body: %v", err))
			return
		}
		if len(body.Videos) == 0 {
			s.writeError(w, 400, "videos list required")
			return
		}
		pl := &playlist.Playlist{
			Name:   body.Name,
			Videos: body.Videos,
		}
		if pl.Name == "" {
			pl.Name = "api-playlist"
		}
		s.worker.SetPlaylist(pl)
		log.Printf("[api] Replaced playlist with %d videos from API", len(pl.Videos))
		s.writeJSON(w, map[string]any{"status": "ok", "count": len(pl.Videos)})

	default:
		s.writeError(w, 405, "GET or POST required")
	}
}

// ── YouTube playlist resolution ─────────────────────────────────────

type playlistAddBody struct {
	URL string `json:"url"`
}

func (s *Server) handlePlaylistAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body playlistAddBody
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, fmt.Sprintf("invalid body: %v", err))
		return
	}
	if body.URL == "" {
		s.writeError(w, 400, "url required")
		return
	}

	videos, plName, err := s.resolveYouTubePlaylist(body.URL)
	if err != nil {
		s.writeError(w, 500, fmt.Sprintf("resolve playlist: %v", err))
		return
	}

	pl := &playlist.Playlist{
		Name:   plName,
		Videos: videos,
	}
	s.worker.SetPlaylist(pl)
	log.Printf("[api] Loaded %d videos from YouTube playlist %q", len(videos), plName)
	s.writeJSON(w, map[string]any{"status": "ok", "count": len(videos), "name": plName})
}

// resolveYouTubePlaylist uses yt-dlp --flat-playlist --dump-json to enumerate.
// Returns video entries, a human-readable playlist name, and any error.
func (s *Server) resolveYouTubePlaylist(playlistURL string) ([]playlist.VideoEntry, string, error) {
	ytdlp := s.cfg.Streamer.YtdlpPath
	if ytdlp == "" {
		ytdlp = "yt-dlp"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	args := []string{
		"--flat-playlist",
		"--dump-json",
		"--no-warnings",
		"--ignore-errors",
		"--compat-options", "no-youtube-unavailable-videos",
	}
	if s.cfg.Streamer.CookiesFile != "" {
		args = append(args, "--cookies", s.cfg.Streamer.CookiesFile)
	}
	if s.cfg.Streamer.CookiesFromBrowser != "" {
		args = append(args, "--cookies-from-browser", s.cfg.Streamer.CookiesFromBrowser)
	}
	args = append(args, playlistURL)

	cmd := exec.CommandContext(ctx, ytdlp, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, "", fmt.Errorf("yt-dlp: %w (stderr: %s)", err, string(ee.Stderr))
		}
		return nil, "", fmt.Errorf("yt-dlp: %w", err)
	}

	var videos []playlist.VideoEntry
	plName := "YouTube Playlist"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			ID            string `json:"id"`
			URL           string `json:"url"`
			Title         string `json:"title"`
			PlaylistTitle string `json:"playlist_title"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			log.Printf("[api] yt-dlp line parse error: %v (line: %.80s)", err, line)
			continue
		}
		if entry.PlaylistTitle != "" && plName == "YouTube Playlist" {
			plName = entry.PlaylistTitle
		}
		videoURL := entry.URL
		if videoURL == "" && entry.ID != "" {
			videoURL = "https://www.youtube.com/watch?v=" + entry.ID
		}
		if videoURL == "" {
			continue
		}
		videos = append(videos, playlist.VideoEntry{
			URL:      videoURL,
			Provider: "youtube",
		})
	}
	if len(videos) == 0 {
		return nil, "", fmt.Errorf("no videos found in playlist")
	}
	return videos, plName, nil
}

// ── Single video add / remove ───────────────────────────────────────

type videoBody struct {
	URL      string `json:"url"`
	Provider string `json:"provider"` // optional, inferred if empty
}

func (s *Server) handleAddVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body videoBody
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, fmt.Sprintf("invalid body: %v", err))
		return
	}
	if body.URL == "" {
		s.writeError(w, 400, "url required")
		return
	}
	if body.Provider == "" {
		body.Provider = providers.InferProviderFromURL(body.URL)
	}
	if body.Provider == "" {
		body.Provider = "youtube"
	}

	added := false
	s.worker.WithPlaylist(func(pl *playlist.Playlist) {
		if pl != nil {
			pl.Videos = append(pl.Videos, playlist.VideoEntry{
				URL:      body.URL,
				Provider: body.Provider,
			})
			added = true
		}
	})

	if !added {
		s.writeError(w, 400, "no playlist loaded")
		return
	}
	log.Printf("[api] Added video: %s [%s]", body.URL, body.Provider)
	s.writeJSON(w, map[string]any{"status": "ok", "provider": body.Provider})
}

type removeVideoBody struct {
	Index int `json:"index"`
}

func (s *Server) handleRemoveVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		s.writeError(w, 405, "POST or DELETE required")
		return
	}
	var body removeVideoBody
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, fmt.Sprintf("invalid body: %v", err))
		return
	}

	removed := false
	s.worker.WithPlaylist(func(pl *playlist.Playlist) {
		if pl != nil && body.Index >= 0 && body.Index < len(pl.Videos) {
			pl.Videos = append(pl.Videos[:body.Index], pl.Videos[body.Index+1:]...)
			removed = true
		}
	})

	if !removed {
		s.writeError(w, 400, "invalid index or no playlist")
		return
	}
	s.writeJSON(w, map[string]string{"status": "removed"})
}

// ── Load/save playlist files ────────────────────────────────────────

type loadPlaylistBody struct {
	File string `json:"file"`
}

func (s *Server) handleLoadPlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body loadPlaylistBody
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, fmt.Sprintf("invalid body: %v", err))
		return
	}
	if body.File == "" {
		s.writeError(w, 400, "file required")
		return
	}

	pf, err := playlist.LoadPlaylistFromDir(s.playlistDir, body.File)
	if err != nil {
		s.writeError(w, 500, fmt.Sprintf("load playlist: %v", err))
		return
	}
	pl := pf.First()
	if pl == nil {
		s.writeError(w, 500, "empty playlist file")
		return
	}
	s.worker.SetPlaylist(pl)
	log.Printf("[api] Loaded playlist file %s (%d videos)", body.File, len(pl.Videos))
	s.writeJSON(w, map[string]any{"status": "ok", "name": pl.Name, "count": len(pl.Videos)})
}

// ── Cookie upload ─────────────────────────────────────────────

func (s *Server) handleUploadCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}

	// Accept multipart file upload (from browser extension) or raw text body
	var data []byte

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			s.writeError(w, 400, "parse multipart")
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			// Try "cookies.txt" as field name too
			file, _, err = r.FormFile("cookies.txt")
		}
		if err != nil {
			s.writeError(w, 400, "no file in upload (use field name 'file' or 'cookies.txt')")
			return
		}
		defer file.Close()
		data, err = io.ReadAll(file)
		if err != nil {
			s.writeError(w, 500, "read upload")
			return
		}
	} else {
		// Raw body — curl -X POST --data-binary @cookies.txt
		var err error
		data, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			s.writeError(w, 500, "read body")
			return
		}
	}

	if len(data) == 0 {
		s.writeError(w, 400, "empty file")
		return
	}

	cookiesPath := s.cfg.Streamer.CookiesFile
	if cookiesPath == "" {
		cookiesPath = "/opt/owncast/playlist-streamer/cookies.txt"
	}

	// Write to a temp first for safety, then rename
	tmpPath := cookiesPath + ".new"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		s.writeError(w, 500, fmt.Sprintf("write temp: %v", err))
		return
	}
	if err := os.Rename(tmpPath, cookiesPath); err != nil {
		os.Remove(tmpPath)
		s.writeError(w, 500, fmt.Sprintf("rename: %v", err))
		return
	}

	lineCount := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lineCount++
		}
	}

	log.Printf("[api] Cookies replaced (%d entries) at %s", lineCount, cookiesPath)
	s.writeJSON(w, map[string]any{"status": "ok", "entries": lineCount})
}

// ── Video upload ────────────────────────────────────────────

var videosDir = "/opt/owncast/playlist-streamer/videos"

func (s *Server) handleUploadVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}

	if err := r.ParseMultipartForm(2 << 30); err != nil { // 2GB max
		s.writeError(w, 400, fmt.Sprintf("parse form: %v", err))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		s.writeError(w, 400, fmt.Sprintf("no file field: %v", err))
		return
	}
	defer file.Close()

	// Sanitize filename
	name := strings.ReplaceAll(header.Filename, " ", "-")
	name = strings.Map(func(r rune) rune {
		if r >= 32 && r <= 126 && r != '/' && r != '\\' {
			return r
		}
		return -1
	}, name)
	if name == "" {
		name = fmt.Sprintf("upload-%d.mp4", time.Now().Unix())
	}

	outPath := videosDir + "/" + name
	out, err := os.Create(outPath)
	if err != nil {
		s.writeError(w, 500, fmt.Sprintf("create file: %v", err))
		return
	}
	defer out.Close()

	written, err := io.Copy(out, file)
	if err != nil {
		os.Remove(outPath)
		s.writeError(w, 500, fmt.Sprintf("write file: %v", err))
		return
	}

	// Add to current playlist
	added := false
	s.worker.WithPlaylist(func(pl *playlist.Playlist) {
		if pl != nil {
			pl.Videos = append(pl.Videos, playlist.VideoEntry{
				URL:      outPath,
				Provider: "local",
			})
			added = true
		}
	})

	log.Printf("[api] Uploaded %s (%d MB), added to playlist", name, written/(1024*1024))
	s.writeJSON(w, map[string]any{
		"status":          "ok",
		"file":            name,
		"path":            outPath,
		"bytes":           written,
		"addedToPlaylist": added,
	})

	// Wake the streamer up — it may be in a holding pattern
	s.worker.Send(worker.CmdRewind)
}

func (s *Server) handleListVideos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "GET required")
		return
	}
	entries, err := os.ReadDir(videosDir)
	if err != nil {
		s.writeError(w, 500, fmt.Sprintf("read dir: %v", err))
		return
	}
	var files []map[string]any
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, _ := e.Info()
		files = append(files, map[string]any{
			"name":    e.Name(),
			"size":    fi.Size(),
			"modTime": fi.ModTime(),
		})
	}
	if files == nil {
		files = []map[string]any{}
	}
	s.writeJSON(w, files)
}

func (s *Server) handleSavePlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}

	pl := s.worker.CurrentPlaylist()
	if pl == nil {
		s.writeError(w, 400, "no playlist to save")
		return
	}

	if s.playlistDir == "" {
		s.writeError(w, 500, "no playlists directory configured")
		return
	}

	ts := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("api-%s.yaml", ts)
	writePath := s.playlistDir + "/" + filename

	pf := &playlist.PlaylistFile{
		Playlists: []playlist.Playlist{*pl},
	}
	data, err := yaml.Marshal(pf)
	if err != nil {
		s.writeError(w, 500, fmt.Sprintf("marshal: %v", err))
		return
	}
	if err := os.WriteFile(writePath, data, 0644); err != nil {
		s.writeError(w, 500, fmt.Sprintf("write: %v", err))
		return
	}
	log.Printf("[api] Saved playlist to %s", writePath)
	s.writeJSON(w, map[string]string{"status": "ok", "file": filename})
}

// ── Schedule ──────────────────────────────────────────────────────

// currentSchedule holds the in-memory schedule, loaded from a file or created via API.
var currentSchedule *playlist.Schedule
var scheduleMu sync.Mutex

func (s *Server) getSchedulePath(filename string) string {
	if filename != "" {
		return s.playlistDir + "/" + filename
	}
	return s.playlistDir + "/schedule.yaml"
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		scheduleMu.Lock()
		sch := currentSchedule
		scheduleMu.Unlock()
		if sch == nil {
			// Try loading from default file
			loaded, err := playlist.LoadSchedule(s.getSchedulePath(""))
			if err != nil {
				sch = &playlist.Schedule{Name: "New Schedule"}
			} else {
				sch = loaded
				scheduleMu.Lock()
				currentSchedule = sch
				scheduleMu.Unlock()
			}
		}
		s.writeJSON(w, sch)

	case http.MethodPost:
		var sch playlist.Schedule
		if err := s.decodeBody(r, &sch); err != nil {
			s.writeError(w, 400, fmt.Sprintf("invalid schedule: %v", err))
			return
		}
		scheduleMu.Lock()
		currentSchedule = &sch
		scheduleMu.Unlock()
		log.Printf("[api] Schedule updated: %q (%d entries)", sch.Name, len(sch.Entries))
		s.writeJSON(w, map[string]any{"status": "ok", "entries": len(sch.Entries)})

	default:
		s.writeError(w, 405, "GET or POST required")
	}
}

func (s *Server) handleScheduleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var entry playlist.ScheduleEntry
	if err := s.decodeBody(r, &entry); err != nil {
		s.writeError(w, 400, fmt.Sprintf("invalid entry: %v", err))
		return
	}
	if entry.Time == "" {
		s.writeError(w, 400, "time required (HH:MM)")
		return
	}

	scheduleMu.Lock()
	if currentSchedule == nil {
		currentSchedule = &playlist.Schedule{Name: "Schedule"}
	}
	currentSchedule.AddEntry(entry)
	sch := currentSchedule
	scheduleMu.Unlock()

	log.Printf("[api] Added schedule entry at %s", entry.Time)
	s.writeJSON(w, map[string]any{"status": "ok", "entries": len(sch.Entries)})
}

func (s *Server) handleScheduleRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body struct {
		Index int `json:"index"`
	}
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, fmt.Sprintf("invalid body: %v", err))
		return
	}

	scheduleMu.Lock()
	if currentSchedule == nil {
		scheduleMu.Unlock()
		s.writeError(w, 400, "no schedule loaded")
		return
	}
	err := currentSchedule.RemoveEntry(body.Index)
	sch := currentSchedule
	scheduleMu.Unlock()

	if err != nil {
		s.writeError(w, 400, err.Error())
		return
	}
	log.Printf("[api] Removed schedule entry at index %d", body.Index)
	s.writeJSON(w, map[string]any{"status": "ok", "entries": len(sch.Entries)})
}

func (s *Server) handleScheduleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}

	scheduleMu.Lock()
	sch := currentSchedule
	scheduleMu.Unlock()

	if sch == nil {
		s.writeError(w, 400, "no schedule to save")
		return
	}

	if s.playlistDir == "" {
		s.writeError(w, 500, "no playlists directory configured")
		return
	}

	ts := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("schedule-%s.yaml", ts)
	writePath := s.playlistDir + "/" + filename

	if err := playlist.SaveSchedule(writePath, sch); err != nil {
		s.writeError(w, 500, fmt.Sprintf("save: %v", err))
		return
	}
	log.Printf("[api] Saved schedule to %s", writePath)
	s.writeJSON(w, map[string]string{"status": "ok", "file": filename})
}

func (s *Server) handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	if s.isScheduleRunning() {
		s.writeJSON(w, map[string]string{"status": "already running"})
		return
	}
	s.startScheduleRunner()
	s.writeJSON(w, map[string]string{"status": "running"})
}

func (s *Server) handleScheduleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	s.stopScheduleRunner()
	s.writeJSON(w, map[string]string{"status": "stopped"})
}

func (s *Server) handleScheduleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "GET required")
		return
	}
	s.writeJSON(w, map[string]any{
		"running": s.isScheduleRunning(),
	})
}
