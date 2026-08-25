package applog

import (
	"strings"
	"sync"
)

type Ring struct {
	mu      sync.RWMutex
	lines   []string
	partial string
	max     int
}

var Default = &Ring{max: 500}

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := r.partial + string(p)
	parts := strings.Split(text, "\n")
	r.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		if line != "" {
			r.lines = append(r.lines, line)
		}
	}
	if len(r.lines) > r.max {
		r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

func (r *Ring) Lines(limit int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 || limit > len(r.lines) {
		limit = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-limit:]...)
}
