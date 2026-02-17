package playlist

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PlaylistFile represents a YAML playlist file.
type PlaylistFile struct {
	Playlists []Playlist `yaml:"playlists"`
}

// Playlist is a named list of videos with optional schedule.
type Playlist struct {
	Name     string        `yaml:"name"`
	Videos   []VideoEntry  `yaml:"videos"`
	Schedule string        `yaml:"schedule"` // cron expression, optional
}

// VideoEntry is a single video in a playlist.
type VideoEntry struct {
	URL      string `yaml:"url"`
	Provider string `yaml:"provider"` // youtube, kick, twitch
}

// LoadPlaylist loads a playlist from a YAML file.
func LoadPlaylist(path string) (*PlaylistFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pf PlaylistFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, err
	}
	return &pf, nil
}

// LoadPlaylistFromDir loads the first playlist found in dir, or the named file.
func LoadPlaylistFromDir(dir string, name string) (*PlaylistFile, error) {
	if name != "" {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return LoadPlaylist(p)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		pf, err := LoadPlaylist(p)
		if err != nil {
			continue
		}
		if len(pf.Playlists) > 0 {
			return pf, nil
		}
	}
	return nil, fmt.Errorf("no playlist found in %s", dir)
}

// First returns the first playlist, or nil if none.
func (pf *PlaylistFile) First() *Playlist {
	if len(pf.Playlists) == 0 {
		return nil
	}
	return &pf.Playlists[0]
}
