package worker

import (
	"strings"
	"sync"
	"time"

	"playlist-streamer/playlist"
	"playlist-streamer/providers"
)

// MediaItemStatus is the operator-facing preparation state for one playlist
// item. It deliberately contains no resolved URLs or filesystem paths so the
// dashboard cannot expose Plex tokens or server-side mount details.
type MediaItemStatus struct {
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	Bytes     int64     `json:"bytes,omitempty"`
	Total     int64     `json:"totalBytes,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type mediaStatusStore struct {
	mu    sync.RWMutex
	items map[string]MediaItemStatus
}

func (s *mediaStatusStore) reset(pl *playlist.Playlist, cacheEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make(map[string]MediaItemStatus)
	if pl == nil {
		return
	}
	for _, entry := range pl.Videos {
		providerName := entry.Provider
		if providerName == "" {
			providerName = providers.InferProviderFromURL(entry.URL)
		}
		state, detail := "direct", "Streams directly"
		switch providerName {
		case "local":
			state, detail = "ready", "Stored on Couch"
		case "plex", "smb":
			if cacheEnabled {
				state, detail = "waiting", "Not cached yet"
			}
		}
		s.items[entry.URL] = MediaItemStatus{State: state, Detail: detail, UpdatedAt: time.Now()}
	}
}

func (s *mediaStatusStore) set(key, state, detail string, bytes, total int64) {
	if key == "" {
		return
	}
	detail = strings.TrimSpace(strings.ReplaceAll(detail, "\n", " "))
	if len(detail) > 240 {
		detail = detail[:237] + "..."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]MediaItemStatus)
	}
	s.items[key] = MediaItemStatus{
		State: state, Detail: detail, Bytes: bytes, Total: total, UpdatedAt: time.Now(),
	}
}

func (s *mediaStatusStore) snapshot() map[string]MediaItemStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copy := make(map[string]MediaItemStatus, len(s.items))
	for key, status := range s.items {
		copy[key] = status
	}
	return copy
}
