package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/providers"
)

// sanitizePlaylistFilename turns a torrent name into a filesystem-safe base name.
// Replaces problematic chars, collapses dashes, limits length. For unreadable
// names (hashes, etc.) we try to extract something meaningful.
func sanitizePlaylistFilename(name string) string {
	if name == "" {
		return "unnamed"
	}
	// Remove common video extensions to get cleaner base
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range []string{".mkv", ".mp4", ".avi", ".webm", ".mov", ".wmv"} {
		if ext == e {
			name = strings.TrimSuffix(name, filepath.Ext(name))
			break
		}
	}
	// Replace filesystem-unsafe chars with dash
	unsafe := regexp.MustCompile(`[\/\\:*?"<>|]`)
	name = unsafe.ReplaceAllString(name, "-")
	// Replace dots and underscores with dash for consistency
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, "_", "-")
	// Collapse multiple dashes
	multiDash := regexp.MustCompile(`-+`)
	name = multiDash.ReplaceAllString(name, "-")
	// Trim and limit length
	name = strings.Trim(name, " -")
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		return "unnamed"
	}
	return name
}

// ImportFromRealDebrid fetches cached torrents from Real-Debrid and creates
// playlist files. Returns (createdCount, error).
func ImportFromRealDebrid(playlistDir, configPath string) (int, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return 0, fmt.Errorf("load config: %w", err)
	}
	if cfg.Streamer.RealDebridToken == "" {
		return 0, fmt.Errorf("realdebrid_token not set in config")
	}
	rd := providers.NewRealDebrid()
	rd.SetAPIToken(cfg.Streamer.RealDebridToken)

	torrents, err := rd.ListCachedTorrents()
	if err != nil {
		return 0, fmt.Errorf("list torrents: %w", err)
	}
	if len(torrents) == 0 {
		return 0, nil
	}

	usedNames := make(map[string]bool)
	created := 0
	for _, t := range torrents {
		base := sanitizePlaylistFilename(t.Filename)
		filename := base + ".yaml"
		if usedNames[filename] {
			filename = fmt.Sprintf("%s-%s.yaml", base, t.ID)
		}
		usedNames[filename] = true

		path := filepath.Join(playlistDir, filename)
		// Display name: slightly nicer (spaces instead of dashes)
		displayName := strings.ReplaceAll(base, "-", " ")
		displayName = regexp.MustCompile(`\s+`).ReplaceAllString(displayName, " ")
		displayName = strings.TrimSpace(displayName)
		if displayName == "" {
			displayName = t.Filename
		}

		videos := make([]playlist.VideoEntry, 0, len(t.Links))
		for _, link := range t.Links {
			videos = append(videos, playlist.VideoEntry{
				URL:      link,
				Provider: "realdebrid",
			})
		}

		pf := &playlist.PlaylistFile{
			Playlists: []playlist.Playlist{{
				Name:   displayName,
				Videos: videos,
			}},
		}
		data, err := yaml.Marshal(pf)
		if err != nil {
			return created, fmt.Errorf("marshal %s: %w", filename, err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return created, fmt.Errorf("write %s: %w", filename, err)
		}
		created++
	}
	return created, nil
}
