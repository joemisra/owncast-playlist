package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the main service configuration.
type Config struct {
	Owncast   OwncastConfig  `yaml:"owncast"`
	Streamer  StreamerConfig `yaml:"streamer"`
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
	FFmpegPath         string `yaml:"ffmpeg_path"`          // path to ffmpeg, empty = "ffmpeg"
	YtdlpPath          string `yaml:"ytdlp_path"`           // path to yt-dlp, empty = "yt-dlp"
	CookiesFile        string `yaml:"cookies_file"`         // Netscape cookies.txt
	CookiesFromBrowser string `yaml:"cookies_from_browser"` // e.g. "chrome", "firefox"
	TempDir            string `yaml:"temp_dir"`             // directory for downloads, empty = data/tmp
	Realtime           bool   `yaml:"realtime"`             // use -re for realtime
	LoopPlaylist       bool   `yaml:"loop_playlist"`        // when playlist ends, start over
	MaxRetries         int    `yaml:"max_retries"`          // retries per video before skipping (default 2)
	DelayBetween       int    `yaml:"delay_between"`        // seconds between videos to avoid rate limits (default 10)
	HoldWidth          int    `yaml:"hold_width"`           // blue holding pattern width (default 1280)
	HoldHeight         int    `yaml:"hold_height"`          // blue holding pattern height (default 720)
	HoldFPS            int    `yaml:"hold_fps"`             // holding pattern frame rate (default 30)
	RealDebridToken    string `yaml:"realdebrid_token"`     // Real-Debrid API token from https://real-debrid.com/apitoken
	Subtitles          bool   `yaml:"subtitles"`            // start with subtitles burned into video
	SubtitleLang       string `yaml:"subtitle_lang"`        // subtitle language for yt-dlp downloads (default: "en")
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
	if c.Streamer.MaxRetries == 0 {
		c.Streamer.MaxRetries = 2
	}
	if c.Streamer.DelayBetween == 0 {
		c.Streamer.DelayBetween = 10
	}
	if c.Streamer.HoldWidth == 0 {
		c.Streamer.HoldWidth = 1280
	}
	if c.Streamer.HoldHeight == 0 {
		c.Streamer.HoldHeight = 720
	}
	if c.Streamer.HoldFPS == 0 {
		c.Streamer.HoldFPS = 30
	}
	if c.Streamer.SubtitleLang == "" {
		c.Streamer.SubtitleLang = "en"
	}
}
