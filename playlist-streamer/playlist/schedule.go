package playlist

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// LayerCue holds a command to send to the layer-server at a scheduled time.
type LayerCue struct {
	Command string   `yaml:"command" json:"command"` // "preset", "layer", "fx", "clear", "blend", "opacity"
	Args    []string `yaml:"args" json:"args"`
}

// ScheduleEntry is one time slot in a schedule.
type ScheduleEntry struct {
	Time     string      `yaml:"time" json:"time"`                             // "14:30" (24h wall-clock)
	Duration string      `yaml:"duration,omitempty" json:"duration,omitempty"` // estimated, optional
	Video    *VideoEntry `yaml:"video,omitempty" json:"video,omitempty"`
	Cue      *LayerCue   `yaml:"cue,omitempty" json:"cue,omitempty"`
}

// Schedule holds a day's scheduled entries (sorted by time).
type Schedule struct {
	Name    string          `yaml:"name" json:"name"`
	Entries []ScheduleEntry `yaml:"entries" json:"entries"`
}

// ScheduleFile is the top-level YAML structure.
type ScheduleFile struct {
	Schedule Schedule `yaml:"schedule"`
}

// LoadSchedule loads a schedule YAML file.
func LoadSchedule(path string) (*Schedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sf ScheduleFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse schedule %s: %w", path, err)
	}
	if sf.Schedule.Name == "" {
		sf.Schedule.Name = "Schedule"
	}
	sf.Schedule.sort()
	return &sf.Schedule, nil
}

// LoadScheduleFromDir loads the first schedule found in dir, or the named file.
func LoadScheduleFromDir(dir, name string) (*Schedule, string, error) {
	if name != "" {
		// try with and without .yaml
		for _, suffix := range []string{name, name + ".yaml", name + ".yml"} {
			p := dir + "/" + suffix
			if _, err := os.Stat(p); err == nil {
				s, err := LoadSchedule(p)
				return s, p, err
			}
		}
	}
	// Scan dir for schedule files
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := dir + "/" + e.Name()
		s, err := LoadSchedule(p)
		if err != nil {
			continue
		}
		if s != nil && len(s.Entries) > 0 {
			return s, p, nil
		}
	}
	return nil, "", fmt.Errorf("no schedule found in %s", dir)
}

// SaveSchedule writes a schedule to a YAML file.
func SaveSchedule(path string, s *Schedule) error {
	s.sort()
	sf := ScheduleFile{Schedule: *s}
	data, err := yaml.Marshal(&sf)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (s *Schedule) sort() {
	sort.Slice(s.Entries, func(i, j int) bool {
		return s.Entries[i].Time < s.Entries[j].Time
	})
}

// NextEntryAfter returns the first entry with time > wallClock (HH:MM), or nil.
func (s *Schedule) NextEntryAfter(wallClock string) *ScheduleEntry {
	for i := range s.Entries {
		if s.Entries[i].Time > wallClock {
			return &s.Entries[i]
		}
	}
	return nil
}

// EntryForTime returns the entry whose time matches wallClock (HH:MM), or nil.
func (s *Schedule) EntryForTime(wallClock string) *ScheduleEntry {
	for i := range s.Entries {
		if s.Entries[i].Time == wallClock {
			return &s.Entries[i]
		}
	}
	return nil
}

// AddEntry adds an entry and re-sorts.
func (s *Schedule) AddEntry(e ScheduleEntry) {
	s.Entries = append(s.Entries, e)
	s.sort()
}

// RemoveEntry removes entry at index.
func (s *Schedule) RemoveEntry(idx int) error {
	if idx < 0 || idx >= len(s.Entries) {
		return fmt.Errorf("index %d out of range (0-%d)", idx, len(s.Entries)-1)
	}
	s.Entries = append(s.Entries[:idx], s.Entries[idx+1:]...)
	return nil
}

// TodayEntries returns entries scheduled between from (HH:MM) and to (HH:MM) inclusive.
func (s *Schedule) EntriesInRange(from, to string) []ScheduleEntry {
	var out []ScheduleEntry
	for _, e := range s.Entries {
		if e.Time >= from && e.Time <= to {
			out = append(out, e)
		}
	}
	return out
}

// ParseTime parses "HH:MM" into a time.Time on today in local zone.
func ParseTime(hhmm string) (time.Time, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, err
	}
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local), nil
}
