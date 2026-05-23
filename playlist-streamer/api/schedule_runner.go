package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"playlist-streamer/playlist"
	"playlist-streamer/worker"
)

// scheduleRunner watches the clock and triggers scheduled entries.
type scheduleRunner struct {
	active      atomic.Bool
	stopCh      chan struct{}
	playedToday map[string]bool
}

// startScheduleRunner begins the schedule execution loop.
func (s *Server) startScheduleRunner() {
	s.schedRunner.stopCh = make(chan struct{})
	s.schedRunner.playedToday = make(map[string]bool)
	s.schedRunner.active.Store(true)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		log.Printf("[schedule] Runner started — monitoring schedule")
		for {
			select {
			case <-s.schedRunner.stopCh:
				log.Printf("[schedule] Runner stopped")
				return
			case <-ticker.C:
				s.tickSchedule()
			}
		}
	}()
}

// tickSchedule checks the current time against the schedule.
func (s *Server) tickSchedule() {
	scheduleMu.Lock()
	sch := currentSchedule
	scheduleMu.Unlock()

	if sch == nil || len(sch.Entries) == 0 {
		return
	}

	now := time.Now().Format("15:04")

	// Reset playedToday at midnight
	if now == "00:00" {
		s.schedRunner.playedToday = make(map[string]bool)
	}

	// Find the entry for this exact minute
	entry := sch.EntryForTime(now)
	if entry == nil {
		return
	}

	// Already played this slot today?
	if s.schedRunner.playedToday[now] {
		return
	}
	s.schedRunner.playedToday[now] = true

	log.Printf("[schedule] Triggering entry at %s", now)

	// 1. Fire layer cue if present
	if entry.Cue != nil && entry.Cue.Command != "" {
		if err := s.fireCue(entry.Cue); err != nil {
			log.Printf("[schedule] Cue error: %v", err)
		}
		// Small delay so effect renders before video starts
		time.Sleep(2 * time.Second)
	}

	// 2. Queue the video if present
	if entry.Video != nil {
		pl := &playlist.Playlist{
			Name:   fmt.Sprintf("Scheduled %s", now),
			Videos: []playlist.VideoEntry{*entry.Video},
		}
		s.worker.SetPlaylist(pl)
		// Rewind restarts playback — if the stream is idle (hold mode) this kicks it off
		s.worker.Send(worker.CmdRewind)
	}
}

// fireCue sends a command to the layer-server on localhost:9100.
func (s *Server) fireCue(cue *playlist.LayerCue) error {
	body := map[string]any{
		"cmd":  cue.Command,
		"args": cue.Args,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	resp, err := http.Post("http://localhost:9100/layers-api/state", "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("layer-server unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("layer-server returned %d", resp.StatusCode)
	}

	log.Printf("[schedule] Cue fired: %s %v", cue.Command, cue.Args)
	return nil
}

// stopScheduleRunner halts the schedule execution loop.
func (s *Server) stopScheduleRunner() {
	if s.schedRunner.active.Load() {
		close(s.schedRunner.stopCh)
		s.schedRunner.active.Store(false)
	}
}

// isScheduleRunning returns whether the schedule runner is active.
func (s *Server) isScheduleRunning() bool {
	return s.schedRunner.active.Load()
}
