package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the main service configuration.
type Config struct {
	Owncast   OwncastConfig   `yaml:"owncast"`
	Streamer  StreamerConfig  `yaml:"streamer"`
	Playlists string         `yaml:"playlists"` // path to playlists directory or file
}

// OwncastConfig holds Owncast RTMP connection details.
type OwncastConfig struct {
	RTMPURL   string `yaml:"rtmp_url"`   // e.g. rtmp://localhost:1935/live
	StreamKey string `yaml:"stream_key"` // stream key for ingest
}

// RTMPIngestURL returns the full RTMP URL including stream key.
func (o *OwncastConfig) RTMPIngestURL() string {
	return fmt.Sprintf("%s/%s", o.RTMPURL, o.StreamKey)
}

// StreamerConfig holds ffmpeg and yt-dlp options.
type StreamerConfig struct {
	FFmpegPath    string `yaml:"ffmpeg_path"`     // path to ffmpeg, empty = "ffmpeg"
	YtdlpPath     string `yaml:"ytdlp_path"`      // path to yt-dlp, empty = "yt-dlp"
	CookiesFile   string `yaml:"cookies_file"`     // Netscape cookies.txt - export from logged-in browser to avoid bot detection
	CookiesFromBrowser string `yaml:"cookies_from_browser"` // e.g. "chrome", "firefox" - use if browser is available on server
	TempDir       string `yaml:"temp_dir"`       // directory for downloaded videos before streaming, empty = data/tmp
	Realtime      bool   `yaml:"realtime"`        // use -re to limit to realtime (default true)
	LoopPlaylist  bool   `yaml:"loop_playlist"`   // when playlist ends, start over
}

// Load reads config from path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Owncast.RTMPURL == "" {
		c.Owncast.RTMPURL = "rtmp://localhost:1935/live"
	}
	if c.Streamer.FFmpegPath == "" {
		c.Streamer.FFmpegPath = "ffmpeg"
	}
	if c.Streamer.YtdlpPath == "" {
		c.Streamer.YtdlpPath = "yt-dlp"
	}
	if c.Playlists == "" {
		c.Playlists = "playlists"
	}
	if c.Streamer.TempDir == "" {
		c.Streamer.TempDir = "data/tmp"
	}
}
