package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"playlist-streamer/applog"
	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/providers"
	"playlist-streamer/worker"

	"gopkg.in/yaml.v3"
)

// Server wraps an HTTP listener with a reference to the stream worker.
type Server struct {
	cfg                 *config.Config
	worker              *worker.StreamWorker
	playlistDir         string
	configPath          string
	currentPlaylistFile string
	playlistFileMu      sync.RWMutex
	editMu              sync.RWMutex
	editPlaylist        *playlist.Playlist
	editPlaylistFile    string
	mux                 *http.ServeMux
	srv                 *http.Server
	stopContinuous      atomic.Bool
	schedRunner         scheduleRunner
	plex                *providers.Plex
	restartCh           chan struct{}
}

// New creates a new API server wired to the given worker.
func New(cfg *config.Config, w *worker.StreamWorker, playlistDir, configPath, initialPlaylistFile string, initialPlaylist *playlist.Playlist) *Server {
	plexServers := make([]providers.PlexServer, 0, len(cfg.Plex.Servers))
	for _, server := range cfg.Plex.Servers {
		plexServers = append(plexServers, providers.PlexServer{Name: server.Name, BaseURL: server.BaseURL, Token: server.Token})
	}
	s := &Server{
		cfg:                 cfg,
		worker:              w,
		playlistDir:         playlistDir,
		configPath:          configPath,
		currentPlaylistFile: initialPlaylistFile,
		mux:                 http.NewServeMux(),
		plex:                providers.NewPlex(plexServers),
		restartCh:           make(chan struct{}, 1),
	}
	if initialPlaylist != nil {
		current := *initialPlaylist
		current.Videos = append([]playlist.VideoEntry(nil), initialPlaylist.Videos...)
		s.editPlaylist = &current
		s.editPlaylistFile = initialPlaylistFile
	}
	s.register()
	return s
}

// ShouldContinue returns false when /api/control/stop has been called,
// telling the continuous loop to break instead of restarting the stream.
func (s *Server) ShouldContinue() bool {
	return !s.stopContinuous.Load()
}

func (s *Server) RestartRequests() <-chan struct{} { return s.restartCh }
func (s *Server) requestRestart() {
	s.stopContinuous.Store(false)
	select {
	case s.restartCh <- struct{}{}:
	default:
	}
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
		Handler: s.requireSession(s.dashboard(s.withCORS(s.mux))),
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
	s.mux.HandleFunc("/api/playlists", s.handlePlaylists)
	s.mux.HandleFunc("/api/playlist/move", s.handleMoveVideo)
	s.mux.HandleFunc("/api/playlist/play", s.handlePlayVideo)
	s.mux.HandleFunc("/api/playlist/activate", s.handleActivatePlaylist)
	s.mux.HandleFunc("/api/playlist/add", s.handlePlaylistAdd)
	s.mux.HandleFunc("/api/playlist/video", s.handleAddVideo)
	s.mux.HandleFunc("/api/playlist/remove", s.handleRemoveVideo)
	s.mux.HandleFunc("/api/playlist/load", s.handleLoadPlaylist)
	s.mux.HandleFunc("/api/playlist/save", s.handleSavePlaylist)
	s.mux.HandleFunc("/api/upload", s.handleUploadVideo)
	s.mux.HandleFunc("/api/videos", s.handleListVideos)
	s.mux.HandleFunc("/api/plex/libraries", s.handlePlexLibraries)
	s.mux.HandleFunc("/api/plex/items", s.handlePlexItems)
	s.mux.HandleFunc("/api/config", s.requireConfigAuth(s.handleConfig))
	s.mux.HandleFunc("/api/logs", s.requireConfigAuth(s.handleLogs))
	s.mux.HandleFunc("/api/owncast/title", s.requireConfigAuth(s.handleOwncastTitle))
	s.mux.HandleFunc("/api/admin/cookies", s.requireConfigAuth(s.handleUploadCookies))
	s.mux.HandleFunc("/api/auth/login", s.handleLogin)
	s.mux.HandleFunc("/api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("/api/schedule", s.handleSchedule)
	s.mux.HandleFunc("/api/schedule/add", s.handleScheduleAdd)
	s.mux.HandleFunc("/api/schedule/remove", s.handleScheduleRemove)
	s.mux.HandleFunc("/api/schedule/save", s.handleScheduleSave)
	s.mux.HandleFunc("/api/schedule/run", s.handleScheduleRun)
	s.mux.HandleFunc("/api/schedule/stop", s.handleScheduleStop)
	s.mux.HandleFunc("/api/schedule/status", s.handleScheduleStatus)
}

const sessionDuration = 2 * time.Hour

func (s *Server) sessionSignature(expiry string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.Dashboard.AdminToken))
	mac.Write([]byte(expiry))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validSession(r *http.Request) bool {
	cookie, err := r.Cookie("couch_manager_session")
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= expires {
		return false
	}
	return hmac.Equal([]byte(s.sessionSignature(parts[0])), []byte(parts[1]))
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		public := r.URL.Path == "/login.html" || r.URL.Path == "/login.js" || r.URL.Path == "/reconnect.js" || r.URL.Path == "/api/status" || r.URL.Path == "/api/auth/login"
		if public || s.validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		http.Redirect(w, r, prefixedPath(r, "/login.html"), http.StatusSeeOther)
	})
}

// prefixedPath keeps redirects inside a reverse proxy mount such as /stream.
// Proxies should provide the public mount point in X-Forwarded-Prefix.
func prefixedPath(r *http.Request, target string) string {
	prefix := strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix"))
	if prefix == "" || !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#\\") {
		return target
	}
	prefix = "/" + strings.Trim(prefix, "/")
	if prefix == "/" || strings.Contains(prefix, "//") || strings.Contains(prefix, "/../") || strings.HasSuffix(prefix, "/..") {
		return target
	}
	return prefix + "/" + strings.TrimLeft(target, "/")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, "invalid body")
		return
	}
	expected, supplied := []byte(s.cfg.Dashboard.AdminToken), []byte(body.Password)
	if len(expected) == 0 || subtle.ConstantTimeCompare(expected, supplied) != 1 {
		time.Sleep(250 * time.Millisecond)
		s.writeError(w, 401, "invalid password")
		return
	}
	expires := time.Now().Add(sessionDuration)
	secureCookie := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	expiry := strconv.FormatInt(expires.Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: "couch_manager_session", Value: expiry + "." + s.sessionSignature(expiry), Path: "/", Expires: expires, MaxAge: int(sessionDuration.Seconds()), HttpOnly: true, Secure: secureCookie, SameSite: http.SameSiteStrictMode})
	s.writeJSON(w, map[string]any{"status": "ok", "expiresAt": expires})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "couch_manager_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	s.writeJSON(w, map[string]string{"status": "logged out"})
}

func (s *Server) handleOwncastTitle(w http.ResponseWriter, r *http.Request) {
	const statusURL = "http://127.0.0.1:8080/api/status"
	switch r.Method {
	case http.MethodGet:
		resp, err := http.Get(statusURL)
		if err != nil {
			s.writeError(w, 502, err.Error())
			return
		}
		defer resp.Body.Close()
		var status struct {
			StreamTitle string `json:"streamTitle"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			s.writeError(w, 502, "invalid Owncast status")
			return
		}
		s.writeJSON(w, map[string]string{"title": status.StreamTitle})
	case http.MethodPost:
		var body struct{ Title string }
		if err := s.decodeBody(r, &body); err != nil {
			s.writeError(w, 400, "invalid body")
			return
		}
		body.Title = strings.TrimSpace(body.Title)
		if len(body.Title) > 200 {
			s.writeError(w, 400, "title is too long")
			return
		}
		if err := setOwncastDatastoreString("stream_title", body.Title); err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		log.Printf("[owncast] Stream title updated locally")
		s.writeJSON(w, map[string]string{"status": "updated", "title": body.Title})
	default:
		s.writeError(w, 405, "GET or POST required")
	}
}

func setOwncastDatastoreString(key, value string) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(value); err != nil {
		return fmt.Errorf("encode Owncast value: %w", err)
	}
	encoded := strings.ToUpper(hex.EncodeToString(buf.Bytes()))
	query := fmt.Sprintf("insert into datastore(key,value) values('%s', X'%s') on conflict(key) do update set value=excluded.value,timestamp=CURRENT_TIMESTAMP;", key, encoded)
	if out, err := exec.Command("sqlite3", "/opt/owncast/owncast/data/owncast.db", query).CombinedOutput(); err != nil {
		return fmt.Errorf("update Owncast datastore: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "GET required")
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}
	s.writeJSON(w, map[string]any{"lines": applog.Default.Lines(limit)})
}

func (s *Server) requireConfigAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.validSession(r) {
			s.writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		next(w, r)
	}
}

type configServerResp struct {
	Name     string `json:"name"`
	BaseURL  string `json:"baseUrl"`
	HasToken bool   `json:"hasToken"`
}

type configResp struct {
	Plex struct {
		Servers []configServerResp `json:"servers"`
	} `json:"plex"`
	Streamer struct {
		LoopPlaylist       bool   `json:"loopPlaylist"`
		Realtime           bool   `json:"realtime"`
		MaxRetries         int    `json:"maxRetries"`
		DelayBetween       int    `json:"delayBetween"`
		Subtitles          bool   `json:"subtitles"`
		SubtitleLang       string `json:"subtitleLang"`
		PlexCacheEnabled   bool   `json:"plexCacheEnabled"`
		PlexCacheMaxGB     int64  `json:"plexCacheMaxGB"`
		PlexCacheMinFreeGB int64  `json:"plexCacheMinFreeGB"`
	} `json:"streamer"`
	YouTube struct {
		HasCookies bool `json:"hasCookies"`
	} `json:"youtube"`
}

type configUpdateBody struct {
	Plex struct {
		Servers []struct{ Name, BaseURL, Token string } `json:"servers"`
	} `json:"plex"`
	Streamer struct {
		LoopPlaylist       bool   `json:"loopPlaylist"`
		Realtime           bool   `json:"realtime"`
		MaxRetries         int    `json:"maxRetries"`
		DelayBetween       int    `json:"delayBetween"`
		Subtitles          bool   `json:"subtitles"`
		SubtitleLang       string `json:"subtitleLang"`
		PlexCacheEnabled   bool   `json:"plexCacheEnabled"`
		PlexCacheMaxGB     int64  `json:"plexCacheMaxGB"`
		PlexCacheMinFreeGB int64  `json:"plexCacheMinFreeGB"`
	} `json:"streamer"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var response configResp
		for _, server := range s.cfg.Plex.Servers {
			response.Plex.Servers = append(response.Plex.Servers, configServerResp{Name: server.Name, BaseURL: server.BaseURL, HasToken: server.Token != ""})
		}
		response.Streamer.LoopPlaylist = s.cfg.Streamer.LoopPlaylist
		response.Streamer.Realtime = s.cfg.Streamer.Realtime
		response.Streamer.MaxRetries = s.cfg.Streamer.MaxRetries
		response.Streamer.DelayBetween = s.cfg.Streamer.DelayBetween
		response.Streamer.Subtitles = s.cfg.Streamer.Subtitles
		response.Streamer.SubtitleLang = s.cfg.Streamer.SubtitleLang
		response.Streamer.PlexCacheEnabled = s.cfg.Streamer.PlexCacheEnabled
		response.Streamer.PlexCacheMaxGB = s.cfg.Streamer.PlexCacheMaxGB
		response.Streamer.PlexCacheMinFreeGB = s.cfg.Streamer.PlexCacheMinFreeGB
		response.YouTube.HasCookies = s.cfg.Streamer.CookiesFile != ""
		s.writeJSON(w, response)
	case http.MethodPost:
		var body configUpdateBody
		if err := s.decodeBody(r, &body); err != nil {
			s.writeError(w, 400, "invalid settings body")
			return
		}
		if err := s.applyConfigUpdate(body); err != nil {
			s.writeError(w, 400, err.Error())
			return
		}
		if err := s.saveConfig(); err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, map[string]any{"status": "saved", "restarting": true})
		go func() { time.Sleep(350 * time.Millisecond); os.Exit(3) }()
	default:
		s.writeError(w, 405, "GET or POST required")
	}
}

func (s *Server) applyConfigUpdate(body configUpdateBody) error {
	existingTokens := make(map[string]string)
	for _, server := range s.cfg.Plex.Servers {
		existingTokens[server.Name] = server.Token
	}
	seen := make(map[string]bool)
	servers := make([]config.PlexServerConfig, 0, len(body.Plex.Servers))
	for _, input := range body.Plex.Servers {
		name := strings.TrimSpace(input.Name)
		baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
		if name == "" || baseURL == "" {
			return fmt.Errorf("each Plex server requires a name and base URL")
		}
		if seen[name] {
			return fmt.Errorf("duplicate Plex server name %q", name)
		}
		parsed, err := url.Parse(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("invalid Plex URL for %q", name)
		}
		token := strings.TrimSpace(input.Token)
		if token == "" {
			token = existingTokens[name]
		}
		if token == "" {
			return fmt.Errorf("Plex server %q requires a token", name)
		}
		seen[name] = true
		servers = append(servers, config.PlexServerConfig{Name: name, BaseURL: baseURL, Token: token})
	}
	if body.Streamer.MaxRetries < 1 || body.Streamer.MaxRetries > 20 {
		return fmt.Errorf("max retries must be between 1 and 20")
	}
	if body.Streamer.DelayBetween < 0 || body.Streamer.DelayBetween > 3600 {
		return fmt.Errorf("delay must be between 0 and 3600 seconds")
	}
	if body.Streamer.PlexCacheEnabled && (body.Streamer.PlexCacheMaxGB < 1 || body.Streamer.PlexCacheMaxGB > 1000) {
		return fmt.Errorf("Plex cache limit must be between 1 and 1000 GB")
	}
	if body.Streamer.PlexCacheEnabled && (body.Streamer.PlexCacheMinFreeGB < 1 || body.Streamer.PlexCacheMinFreeGB > 1000) {
		return fmt.Errorf("Plex cache disk reserve must be between 1 and 1000 GB")
	}
	s.cfg.Plex.Servers = servers
	s.cfg.Streamer.LoopPlaylist = body.Streamer.LoopPlaylist
	s.cfg.Streamer.Realtime = body.Streamer.Realtime
	s.cfg.Streamer.MaxRetries = body.Streamer.MaxRetries
	s.cfg.Streamer.DelayBetween = body.Streamer.DelayBetween
	s.cfg.Streamer.Subtitles = body.Streamer.Subtitles
	s.cfg.Streamer.SubtitleLang = strings.TrimSpace(body.Streamer.SubtitleLang)
	s.cfg.Streamer.PlexCacheEnabled = body.Streamer.PlexCacheEnabled
	s.cfg.Streamer.PlexCacheMaxGB = body.Streamer.PlexCacheMaxGB
	s.cfg.Streamer.PlexCacheMinFreeGB = body.Streamer.PlexCacheMinFreeGB
	if s.cfg.Streamer.SubtitleLang == "" {
		s.cfg.Streamer.SubtitleLang = "en"
	}
	return nil
}

func (s *Server) saveConfig() error {
	data, err := yaml.Marshal(s.cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp := s.configPath + ".new"
	if err := os.WriteFile(tmp, data, 0620); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, s.configPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func (s *Server) handlePlexLibraries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	libraries, err := s.plex.ListLibraries()
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.writeJSON(w, libraries)
}

func (s *Server) handlePlexItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	items, err := s.plex.ListItems(r.URL.Query().Get("server"), r.URL.Query().Get("section"), r.URL.Query().Get("type"))
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.writeJSON(w, items)
}

// ── CORS ────────────────────────────────────────────────────────────

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
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
	Phase        string `json:"phase"`
	CacheBytes   int64  `json:"cacheBytes,omitempty"`
	CacheTotal   int64  `json:"cacheTotalBytes,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "GET required")
		return
	}
	name, videos, curIdx := s.worker.PlaylistSnapshot()
	cacheBytes, cacheTotal := s.worker.CacheProgress()
	resp := statusResp{
		Playing:      s.worker.IsPlaying(),
		Paused:       s.worker.IsPaused(),
		Subtitles:    s.worker.SubtitlesEnabled(),
		CurrentURL:   s.worker.CurrentURL(),
		CurrentIndex: curIdx,
		TotalVideos:  len(videos),
		PlaylistName: name,
		Phase:        s.worker.Phase(),
		CacheBytes:   cacheBytes,
		CacheTotal:   cacheTotal,
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
	if !s.worker.IsPlaying() {
		s.requestRestart()
		log.Printf("[api] Stream restart requested")
		s.writeJSON(w, map[string]string{"status": "starting"})
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
	File         string                `json:"file"`
	ActiveFile   string                `json:"activeFile"`
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pl, file := s.editorSnapshot()
		name, videos := "", []playlist.VideoEntry{}
		if pl != nil {
			name = pl.Name
			videos = append(videos, pl.Videos...)
		}
		curIdx := -1
		if file == s.getCurrentPlaylistFile() {
			curIdx = s.worker.CurrentIndex()
		}
		s.writeJSON(w, playlistResp{
			Name:         name,
			Videos:       videos,
			CurrentIndex: curIdx,
			File:         file,
			ActiveFile:   s.getCurrentPlaylistFile(),
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
		_, file := s.editorSnapshot()
		s.setEditor(pl, file)
		log.Printf("[api] Replaced playlist with %d videos from API", len(pl.Videos))
		s.writeJSON(w, map[string]any{"status": "ok", "count": len(pl.Videos)})

	default:
		s.writeError(w, 405, "GET or POST required")
	}
}

func (s *Server) editorSnapshot() (*playlist.Playlist, string) {
	s.editMu.RLock()
	defer s.editMu.RUnlock()
	if s.editPlaylist == nil {
		return nil, s.editPlaylistFile
	}
	cp := *s.editPlaylist
	cp.Videos = append([]playlist.VideoEntry(nil), s.editPlaylist.Videos...)
	return &cp, s.editPlaylistFile
}

func (s *Server) setEditor(pl *playlist.Playlist, file string) {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	s.editPlaylist = pl
	s.editPlaylistFile = file
}

func (s *Server) withEditor(fn func(*playlist.Playlist)) bool {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if s.editPlaylist == nil {
		return false
	}
	fn(s.editPlaylist)
	return true
}

func (s *Server) handleActivatePlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	pl, file := s.editorSnapshot()
	if pl == nil || len(pl.Videos) == 0 {
		s.writeError(w, 400, "playlist is empty")
		return
	}
	s.worker.SetPlaylist(pl)
	s.setCurrentPlaylistFile(file)
	if !s.worker.IsPlaying() {
		s.requestRestart()
	}
	log.Printf("[playlist] Activated %s (%q, %d items)", file, pl.Name, len(pl.Videos))
	s.writeJSON(w, map[string]any{"status": "activated", "file": file, "count": len(pl.Videos)})
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
	_, file := s.editorSnapshot()
	s.setEditor(pl, file)
	log.Printf("[playlist] Imported %d YouTube items into editor playlist %q", len(videos), plName)
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
			Title:    entry.Title,
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
	Title    string `json:"title"`
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

	added := s.withEditor(func(pl *playlist.Playlist) {
		pl.Videos = append(pl.Videos, playlist.VideoEntry{
			URL:      body.URL,
			Provider: body.Provider,
			Title:    strings.TrimSpace(body.Title),
		})
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
	s.withEditor(func(pl *playlist.Playlist) {
		if body.Index >= 0 && body.Index < len(pl.Videos) {
			pl.Videos = append(pl.Videos[:body.Index], pl.Videos[body.Index+1:]...)
			removed = true
		}
	})
	if !removed {
		s.writeError(w, 400, "invalid playlist index")
		return
	}
	log.Printf("[playlist] Removed item %d", body.Index)
	s.writeJSON(w, map[string]string{"status": "removed"})
}

// ── Load/save playlist files ────────────────────────────────────────

type loadPlaylistBody struct {
	File string `json:"file"`
}

func validPlaylistFile(name string) bool {
	return name != "" && filepath.Base(name) == name && (strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml"))
}

func playlistFilename(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "playlist"
	}
	return base + ".yaml"
}

func (s *Server) getCurrentPlaylistFile() string {
	s.playlistFileMu.RLock()
	defer s.playlistFileMu.RUnlock()
	return s.currentPlaylistFile
}
func (s *Server) setCurrentPlaylistFile(file string) {
	s.playlistFileMu.Lock()
	s.currentPlaylistFile = file
	s.playlistFileMu.Unlock()
}

type playlistSummary struct {
	File    string `json:"file"`
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Current bool   `json:"current"`
	Editing bool   `json:"editing"`
}

func (s *Server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		entries, err := os.ReadDir(s.playlistDir)
		if err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		out := []playlistSummary{}
		_, editingFile := s.editorSnapshot()
		for _, entry := range entries {
			if entry.IsDir() || !validPlaylistFile(entry.Name()) || entry.Name() == "schedule.yaml" {
				continue
			}
			pf, err := playlist.LoadPlaylist(filepath.Join(s.playlistDir, entry.Name()))
			if err != nil || pf.First() == nil {
				continue
			}
			pl := pf.First()
			out = append(out, playlistSummary{File: entry.Name(), Name: pl.Name, Count: len(pl.Videos), Current: entry.Name() == s.getCurrentPlaylistFile(), Editing: entry.Name() == editingFile})
		}
		sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
		s.writeJSON(w, out)
	case http.MethodPost:
		var body struct{ Action, File, Name string }
		if err := s.decodeBody(r, &body); err != nil {
			s.writeError(w, 400, "invalid body")
			return
		}
		switch body.Action {
		case "create":
			name := strings.TrimSpace(body.Name)
			if name == "" {
				s.writeError(w, 400, "name required")
				return
			}
			file := playlistFilename(name)
			path := filepath.Join(s.playlistDir, file)
			if _, err := os.Stat(path); err == nil {
				s.writeError(w, 409, "a playlist with that filename already exists")
				return
			}
			pl := &playlist.Playlist{Name: name, Videos: []playlist.VideoEntry{}}
			if err := writePlaylistFile(path, pl); err != nil {
				s.writeError(w, 500, err.Error())
				return
			}
			s.setEditor(pl, file)
			log.Printf("[playlist] Created editor playlist %s (%q)", file, name)
			s.writeJSON(w, map[string]any{"status": "created", "file": file})
		case "rename":
			if !validPlaylistFile(body.File) {
				s.writeError(w, 400, "invalid file")
				return
			}
			name := strings.TrimSpace(body.Name)
			if name == "" {
				s.writeError(w, 400, "name required")
				return
			}
			pf, err := playlist.LoadPlaylist(filepath.Join(s.playlistDir, body.File))
			if err != nil || pf.First() == nil {
				s.writeError(w, 404, "playlist not found")
				return
			}
			pl := pf.First()
			_, editingFile := s.editorSnapshot()
			if body.File == editingFile {
				pl, _ = s.editorSnapshot()
			}
			pl.Name = name
			newFile := playlistFilename(name)
			if newFile != body.File {
				if _, err := os.Stat(filepath.Join(s.playlistDir, newFile)); err == nil {
					s.writeError(w, 409, "target filename exists")
					return
				}
			}
			if err := writePlaylistFile(filepath.Join(s.playlistDir, newFile), pl); err != nil {
				s.writeError(w, 500, err.Error())
				return
			}
			if newFile != body.File {
				os.Remove(filepath.Join(s.playlistDir, body.File))
			}
			if body.File == editingFile {
				s.setEditor(pl, newFile)
			}
			if body.File == s.getCurrentPlaylistFile() {
				s.setCurrentPlaylistFile(newFile)
			}
			log.Printf("[playlist] Renamed %s to %s (%q)", body.File, newFile, name)
			s.writeJSON(w, map[string]any{"status": "renamed", "file": newFile})
		case "delete":
			if !validPlaylistFile(body.File) {
				s.writeError(w, 400, "invalid file")
				return
			}
			if body.File == s.getCurrentPlaylistFile() {
				s.writeError(w, 409, "cannot delete the playlist currently playing")
				return
			}
			_, editingFile := s.editorSnapshot()
			if body.File == editingFile {
				entries, _ := os.ReadDir(s.playlistDir)
				var fallback string
				for _, entry := range entries {
					if !entry.IsDir() && validPlaylistFile(entry.Name()) && entry.Name() != body.File && entry.Name() != "schedule.yaml" {
						fallback = entry.Name()
						break
					}
				}
				if fallback == "" {
					s.writeError(w, 409, "cannot delete the only playlist")
					return
				}
				pf, err := playlist.LoadPlaylist(filepath.Join(s.playlistDir, fallback))
				if err != nil || pf.First() == nil {
					s.writeError(w, 500, "could not open another playlist")
					return
				}
				s.setEditor(pf.First(), fallback)
			}
			if err := os.Remove(filepath.Join(s.playlistDir, body.File)); err != nil {
				s.writeError(w, 500, err.Error())
				return
			}
			log.Printf("[playlist] Deleted %s", body.File)
			s.writeJSON(w, map[string]string{"status": "deleted"})
		default:
			s.writeError(w, 400, "action must be create, rename, or delete")
		}
	default:
		s.writeError(w, 405, "GET or POST required")
	}
}

func writePlaylistFile(path string, pl *playlist.Playlist) error {
	data, err := yaml.Marshal(&playlist.PlaylistFile{Playlists: []playlist.Playlist{*pl}})
	if err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Server) handleMoveVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body struct{ From, To int }
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, "invalid body")
		return
	}
	moved := false
	s.withEditor(func(pl *playlist.Playlist) {
		if body.From >= 0 && body.From < len(pl.Videos) && body.To >= 0 && body.To < len(pl.Videos) {
			item := pl.Videos[body.From]
			pl.Videos = append(pl.Videos[:body.From], pl.Videos[body.From+1:]...)
			pl.Videos = append(pl.Videos, playlist.VideoEntry{})
			copy(pl.Videos[body.To+1:], pl.Videos[body.To:])
			pl.Videos[body.To] = item
			moved = true
		}
	})
	if !moved {
		s.writeError(w, 400, "invalid playlist index")
		return
	}
	log.Printf("[playlist] Moved editor item %d to %d", body.From, body.To)
	s.writeJSON(w, map[string]any{"status": "moved"})
}

func (s *Server) handlePlayVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "POST required")
		return
	}
	var body struct{ Index int }
	if err := s.decodeBody(r, &body); err != nil {
		s.writeError(w, 400, "invalid body")
		return
	}
	pl, file := s.editorSnapshot()
	videos := pl.Videos
	if body.Index < 0 || body.Index >= len(videos) {
		s.writeError(w, 400, "invalid index")
		return
	}
	if file != s.getCurrentPlaylistFile() {
		s.worker.SetPlaylist(pl)
		s.setCurrentPlaylistFile(file)
	}
	s.worker.JumpTo(body.Index)
	if !s.worker.IsPlaying() {
		s.requestRestart()
	}
	log.Printf("[playlist] Play requested at index %d", body.Index)
	s.writeJSON(w, map[string]any{"status": "playing", "index": body.Index})
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
	if !validPlaylistFile(body.File) {
		s.writeError(w, 400, "invalid file")
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
	s.setEditor(pl, body.File)
	log.Printf("[playlist] Opened %s in editor (%d videos)", body.File, len(pl.Videos))
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
	text := string(data)
	if !strings.Contains(text, "# Netscape HTTP Cookie File") && !strings.Contains(text, ".youtube.com") {
		s.writeError(w, 400, "expected a Netscape cookies.txt export containing YouTube cookies")
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
	s.cfg.Streamer.CookiesFile = cookiesPath
	s.worker.SetYouTubeCookiesFile(cookiesPath)
	if err := s.saveConfig(); err != nil {
		s.writeError(w, 500, fmt.Sprintf("save config: %v", err))
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
	added := s.withEditor(func(pl *playlist.Playlist) {
		pl.Videos = append(pl.Videos, playlist.VideoEntry{
			URL:      outPath,
			Provider: "local",
		})
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

	pl, filename := s.editorSnapshot()
	if pl == nil {
		s.writeError(w, 400, "no playlist to save")
		return
	}

	if s.playlistDir == "" {
		s.writeError(w, 500, "no playlists directory configured")
		return
	}

	if !validPlaylistFile(filename) {
		filename = playlistFilename(pl.Name)
		s.setEditor(pl, filename)
	}
	writePath := filepath.Join(s.playlistDir, filename)
	if err := writePlaylistFile(writePath, pl); err != nil {
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
