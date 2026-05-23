package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/worker"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"
)

type studioViewState int

const (
	studioViewFiles studioViewState = iota
	studioViewPlaylist
	studioViewAddVideo
)

var studioStreamStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("39")).
	BorderStyle(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("238")).
	Padding(0, 1)

type tickMsg struct{}

type studioModel struct {
	cfg         *config.Config
	configPath  string
	playlistDir string

	worker      *worker.StreamWorker
	ctx         context.Context
	cancel      context.CancelFunc
	streamDone  chan error
	streamEnded bool

	files      []string
	fileCursor int

	currentFile *playlist.PlaylistFile
	currentPath string

	videoCursor    int
	providerCursor int
	currentView    studioViewState
	urlInput       textinput.Model

	dirty       bool
	statusMsg   string
	statusIsErr bool
	width       int
	height      int
}

func newStudioModel(cfg *config.Config, configPath, playlistDir string, pf *playlist.PlaylistFile, path string, w *worker.StreamWorker, ctx context.Context, cancel context.CancelFunc, streamDone chan error) *studioModel {
	ti := textinput.New()
	ti.Placeholder = "https://... or magnet:?xt=..."
	ti.CharLimit = 500
	ti.Width = 60

	m := &studioModel{
		cfg:         cfg,
		configPath:  configPath,
		playlistDir: playlistDir,
		worker:      w,
		ctx:         ctx,
		cancel:      cancel,
		streamDone:  streamDone,
		currentFile: pf,
		currentPath: path,
		urlInput:    ti,
	}
	m.loadFiles()
	m.currentView = studioViewPlaylist
	m.videoCursor = 0
	m.setStatus("Streaming — edits apply live; s saves to disk")
	return m
}

func tickStudio() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

func (m *studioModel) loadFiles() {
	m.files = nil
	entries, err := os.ReadDir(m.playlistDir)
	if err != nil {
		m.setError(fmt.Sprintf("cannot read %s: %v", m.playlistDir, err))
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext == ".yaml" || ext == ".yml" {
			m.files = append(m.files, e.Name())
		}
	}
}

func (m *studioModel) setStatus(msg string) { m.statusMsg = msg; m.statusIsErr = false }
func (m *studioModel) setError(msg string)  { m.statusMsg = msg; m.statusIsErr = true }

func (m *studioModel) currentPlaylist() *playlist.Playlist {
	if m.currentFile == nil || len(m.currentFile.Playlists) == 0 {
		return nil
	}
	return &m.currentFile.Playlists[0]
}

func (m *studioModel) withPlaylist(fn func(*playlist.Playlist)) {
	if m.worker != nil {
		m.worker.WithPlaylist(func(wpl *playlist.Playlist) {
			if wpl == nil {
				return
			}
			fn(wpl)
		})
		return
	}
	pl := m.currentPlaylist()
	if pl != nil {
		fn(pl)
	}
}

func (m *studioModel) savePlaylist() error {
	if m.currentFile == nil {
		return fmt.Errorf("no file loaded")
	}
	data, err := yaml.Marshal(m.currentFile)
	if err != nil {
		return err
	}
	return os.WriteFile(m.currentPath, data, 0644)
}

func (m *studioModel) Init() tea.Cmd {
	return tea.Batch(tickStudio(), textinput.Blink)
}

func (m *studioModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.cancel()
			if m.worker != nil {
				m.worker.Send(worker.CmdStop)
			}
			return m, tea.Quit
		}
	case tickMsg:
		if !m.streamEnded {
			select {
			case err := <-m.streamDone:
				m.streamEnded = true
				if err != nil && m.ctx.Err() == nil {
					m.setError(fmt.Sprintf("stream ended: %v", err))
				}
			default:
			}
		}
		return m, tickStudio()
	case importRDResultMsg:
		m.loadFiles()
		if msg.err != nil {
			m.setError(fmt.Sprintf("Import failed: %v", msg.err))
		} else if msg.n == 0 {
			m.setStatus("No cached torrents found (or realdebrid_token not set)")
		} else {
			m.setStatus(fmt.Sprintf("Created %d playlist(s) from Real-Debrid cache", msg.n))
		}
		return m, tickStudio()
	}

	switch m.currentView {
	case studioViewFiles:
		return m.updateStudioFiles(msg)
	case studioViewPlaylist:
		return m.updateStudioPlaylist(msg)
	case studioViewAddVideo:
		return m.updateStudioAddVideo(msg)
	}
	return m, tickStudio()
}

func (m *studioModel) updateStudioFiles(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		if m.fileCursor > 0 {
			m.fileCursor--
		}
	case "down", "j":
		if m.fileCursor < len(m.files)-1 {
			m.fileCursor++
		}
	case "enter":
		if len(m.files) == 0 {
			break
		}
		path := filepath.Join(m.playlistDir, m.files[m.fileCursor])
		pf, err := playlist.LoadPlaylist(path)
		if err != nil {
			m.setError(fmt.Sprintf("load error: %v", err))
			break
		}
		pl := pf.First()
		if pl == nil {
			m.setError("empty playlist file")
			break
		}
		m.currentFile = pf
		m.currentPath = path
		m.currentView = studioViewPlaylist
		m.videoCursor = 0
		m.dirty = false
		if m.worker != nil {
			m.worker.SetPlaylist(pl)
		}
		m.setStatus(fmt.Sprintf("Loaded %s (stream playlist updated)", m.files[m.fileCursor]))
	case "r":
		m.loadFiles()
		m.setStatus("Refreshed")
	case "i":
		if m.cfg == nil {
			m.setError("Config not set — cannot import from Real-Debrid")
			break
		}
		m.setStatus("Importing from Real-Debrid...")
		return m, func() tea.Msg { return runImportRD(m.playlistDir, m.configPath) }
	case "esc", "q":
		m.currentView = studioViewPlaylist
		m.statusMsg = ""
	}
	return m, nil
}

func (m *studioModel) updateStudioPlaylist(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch key.String() {
	case "ctrl+p":
		if m.worker != nil {
			if m.worker.IsPaused() {
				m.worker.Send(worker.CmdPlay)
				m.setStatus("Resumed stream")
			} else {
				m.worker.Send(worker.CmdPause)
				m.setStatus("Pause after current segment")
			}
		}
		return m, nil
	case "ctrl+n":
		if m.worker != nil {
			m.worker.Send(worker.CmdSkip)
			m.setStatus("Skip requested")
		}
		return m, nil
	case "ctrl+u":
		if m.worker != nil {
			if m.worker.SubtitlesEnabled() {
				m.worker.Send(worker.CmdSubsOff)
				m.setStatus("Subtitles off (restarts segment)")
			} else {
				m.worker.Send(worker.CmdSubsOn)
				m.setStatus("Subtitles on (restarts segment)")
			}
		}
		return m, nil
	case "tab":
		m.currentView = studioViewFiles
		m.setStatus("Pick a playlist file (↑↓ enter) — esc returns")
		return m, nil
	}

	pl := m.currentPlaylist()

	switch key.String() {
	case "up", "k":
		if m.videoCursor > 0 {
			m.videoCursor--
		}
	case "down", "j":
		if pl != nil && m.videoCursor < len(pl.Videos)-1 {
			m.videoCursor++
		}
	case "K":
		m.withPlaylist(func(pl *playlist.Playlist) {
			if pl != nil && m.videoCursor > 0 {
				pl.Videos[m.videoCursor], pl.Videos[m.videoCursor-1] = pl.Videos[m.videoCursor-1], pl.Videos[m.videoCursor]
				m.videoCursor--
				m.dirty = true
			}
		})
	case "J":
		m.withPlaylist(func(pl *playlist.Playlist) {
			if pl != nil && m.videoCursor < len(pl.Videos)-1 {
				pl.Videos[m.videoCursor], pl.Videos[m.videoCursor+1] = pl.Videos[m.videoCursor+1], pl.Videos[m.videoCursor]
				m.videoCursor++
				m.dirty = true
			}
		})
	case "a", "n":
		m.currentView = studioViewAddVideo
		m.urlInput.SetValue("")
		m.urlInput.Focus()
		m.providerCursor = 0
		m.statusMsg = ""
		return m, textinput.Blink
	case "d", "x":
		m.withPlaylist(func(pl *playlist.Playlist) {
			if pl != nil && len(pl.Videos) > 0 {
				pl.Videos = append(pl.Videos[:m.videoCursor], pl.Videos[m.videoCursor+1:]...)
				if m.videoCursor >= len(pl.Videos) && m.videoCursor > 0 {
					m.videoCursor--
				}
				m.dirty = true
				m.setStatus("Removed (live)")
			}
		})
	case "s":
		if err := m.savePlaylist(); err != nil {
			m.setError(fmt.Sprintf("save failed: %v", err))
		} else {
			m.dirty = false
			m.setStatus("Saved to disk")
		}
	case "r":
		if m.currentPath != "" {
			pf, err := playlist.LoadPlaylist(m.currentPath)
			if err != nil {
				m.setError(fmt.Sprintf("reload: %v", err))
			} else {
				m.currentFile = pf
				if m.worker != nil {
					if pl := pf.First(); pl != nil {
						m.worker.SetPlaylist(pl)
					}
				}
				m.setStatus("Reloaded from disk & applied to stream")
			}
		}
	case "i":
		m.setStatus("Importing from Real-Debrid...")
		return m, func() tea.Msg { return runImportRD(m.playlistDir, m.configPath) }
	case "q":
		if m.dirty {
			m.setError("Unsaved changes — s to save, ctrl+c to quit")
			break
		}
		m.cancel()
		if m.worker != nil {
			m.worker.Send(worker.CmdStop)
		}
		return m, tea.Quit
	case "Q":
		m.cancel()
		if m.worker != nil {
			m.worker.Send(worker.CmdStop)
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m *studioModel) updateStudioAddVideo(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if ok {
		switch key.Type {
		case tea.KeyEsc:
			m.currentView = studioViewPlaylist
			m.statusMsg = ""
			return m, nil
		case tea.KeyTab:
			m.providerCursor = (m.providerCursor + 1) % len(knownProviders)
			return m, nil
		case tea.KeyShiftTab:
			m.providerCursor = (m.providerCursor - 1 + len(knownProviders)) % len(knownProviders)
			return m, nil
		case tea.KeyEnter:
			url := strings.TrimSpace(m.urlInput.Value())
			if url == "" {
				m.setError("URL cannot be empty")
				return m, nil
			}
			m.withPlaylist(func(pl *playlist.Playlist) {
				if pl != nil {
					pl.Videos = append(pl.Videos, playlist.VideoEntry{
						URL:      url,
						Provider: knownProviders[m.providerCursor],
					})
					m.videoCursor = len(pl.Videos) - 1
					m.dirty = true
					m.setStatus(fmt.Sprintf("Added %s (live)", knownProviders[m.providerCursor]))
				}
			})
			m.currentView = studioViewPlaylist
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.urlInput, cmd = m.urlInput.Update(msg)
	return m, cmd
}

func (m *studioModel) streamPanel(width int) string {
	if m.worker == nil {
		return studioStreamStyle.Width(width).Render("Stream: offline")
	}
	name, _, curIdx := m.worker.PlaylistSnapshot()
	url := m.worker.CurrentURL()
	state := "idle"
	if m.worker.IsPlaying() {
		if m.worker.IsPaused() {
			state = "paused"
		} else {
			state = "playing"
		}
	}
	subs := "off"
	if m.worker.SubtitlesEnabled() {
		subs = "on"
	}
	n := 0
	if pl := m.currentPlaylist(); pl != nil {
		n = len(pl.Videos)
	}
	pos := "-"
	if n > 0 {
		pos = fmt.Sprintf("%d/%d", curIdx+1, n)
	}
	u := truncate(url, width-8)
	if u == "" {
		u = "(holding / gap)"
	}
	if m.streamEnded {
		state = "ended"
	}
	b := strings.Builder{}
	b.WriteString(fmt.Sprintf("Stream  %s  %s  subs:%s\n", state, pos, subs))
	if name != "" {
		b.WriteString(dimStyle.Render(name) + "\n")
	}
	b.WriteString(normalStyle.Render(u))
	return studioStreamStyle.Width(width).Render(b.String())
}

func (m *studioModel) View() string {
	if m.width < 20 {
		m.width = 80
	}
	leftW := (m.width * 62) / 100
	if leftW < 36 {
		leftW = m.width - 32
	}
	if leftW < 30 {
		leftW = 30
	}
	streamW := m.width - leftW - 1
	if streamW < 24 {
		streamW = 24
		leftW = m.width - streamW - 1
	}

	var main strings.Builder
	switch m.currentView {
	case studioViewFiles:
		m.viewStudioFiles(&main, leftW)
	case studioViewPlaylist:
		m.viewStudioPlaylist(&main, leftW)
	case studioViewAddVideo:
		m.viewStudioAddVideo(&main, leftW)
	}

	left := main.String()
	right := m.streamPanel(streamW)
	row := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	var b strings.Builder
	b.WriteString(headerStyle.Render("  Playlist streamer — studio"))
	b.WriteString("\n\n")
	b.WriteString(row)

	if m.statusMsg != "" {
		b.WriteString("\n")
		if m.statusIsErr {
			b.WriteString(errorStyle.Render("  " + m.statusMsg))
		} else {
			b.WriteString(okStyle.Render("  " + m.statusMsg))
		}
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  tab files • ctrl+p pause • ctrl+n skip • ctrl+u subs • s save • q quit • ctrl+c force quit"))
	return b.String()
}

func (m *studioModel) viewStudioFiles(b *strings.Builder, maxW int) {
	b.WriteString(headerStyle.Render("  Open playlist file"))
	b.WriteString("\n\n")
	if len(m.files) == 0 {
		b.WriteString(dimStyle.Render("  No .yaml in " + m.playlistDir))
		b.WriteString("\n")
	} else {
		for i, f := range m.files {
			line := f
			if len(line) > maxW-6 {
				line = line[:maxW-9] + "..."
			}
			if i == m.fileCursor {
				b.WriteString(selectedStyle.Render("  ▸ " + line))
			} else {
				b.WriteString(normalStyle.Render("    " + line))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑↓/jk • enter open • r refresh • i RD import • esc back"))
}

func (m *studioModel) viewStudioPlaylist(b *strings.Builder, maxW int) {
	pl := m.currentPlaylist()
	filename := filepath.Base(m.currentPath)
	mark := ""
	if m.dirty {
		mark = " *"
	}

	b.WriteString(headerStyle.Render(fmt.Sprintf("  %s%s", truncate(filename, maxW-4), mark)))
	b.WriteString("\n\n")

	if pl == nil {
		b.WriteString(dimStyle.Render("  Empty — press a to add"))
		b.WriteString("\n")
		return
	}

	b.WriteString(titleStyle.Render(fmt.Sprintf("  %s", pl.Name)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("  (%d items)", len(pl.Videos))))
	if pl.Schedule != "" {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  cron:%s", pl.Schedule)))
	}
	b.WriteString("\n\n")

	if len(pl.Videos) == 0 {
		b.WriteString(dimStyle.Render("  (empty — a to add)"))
		b.WriteString("\n")
		return
	}

	maxURL := maxW - 28
	if maxURL < 20 {
		maxURL = 20
	}
	name, _, curPlay := m.worker.PlaylistSnapshot()
	_ = name
	for i, v := range pl.Videos {
		num := fmt.Sprintf("%2d.", i+1)
		tag := providerTag(v.Provider)
		url := truncate(v.URL, maxURL)
		marker := "    "
		if i == curPlay && m.worker != nil && m.worker.IsPlaying() && !m.worker.IsPaused() {
			marker = "> "
		} else if i == m.videoCursor {
			marker = " ▸"
		}
		if i == m.videoCursor {
			b.WriteString(selectedStyle.Render(fmt.Sprintf("%s %s ", marker, num)))
			b.WriteString(tag)
			b.WriteString(selectedStyle.Render(" " + url))
		} else {
			b.WriteString(normalStyle.Render(fmt.Sprintf("%s %s ", marker, num)))
			b.WriteString(tag)
			b.WriteString(normalStyle.Render(" " + url))
		}
		b.WriteString("\n")
	}
}

func (m *studioModel) viewStudioAddVideo(b *strings.Builder, maxW int) {
	b.WriteString(headerStyle.Render("  Add video"))
	b.WriteString("\n\n")
	b.WriteString(normalStyle.Render("  URL: "))
	b.WriteString(m.urlInput.View())
	b.WriteString("\n\n")
	b.WriteString(normalStyle.Render("  Provider: "))
	for i, p := range knownProviders {
		if i == m.providerCursor {
			b.WriteString(selectedStyle.Render(fmt.Sprintf(" [%s] ", p)))
		} else {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  %s  ", p)))
		}
	}
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("  tab/shift+tab • enter • esc"))
}

// RunStudio runs the combined playlist editor + stream control TUI.
func RunStudio(cfg *config.Config, configPath, playlistDir string, pf *playlist.PlaylistFile, playlistPath string, w *worker.StreamWorker) error {
	pl := pf.First()
	if pl == nil {
		return fmt.Errorf("no playlist in file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- w.StreamPlaylist(ctx, pl)
	}()

	p := tea.NewProgram(
		newStudioModel(cfg, configPath, playlistDir, pf, playlistPath, w, ctx, cancel, streamDone),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	cancel()
	w.Send(worker.CmdStop)
	select {
	case <-streamDone:
	case <-time.After(8 * time.Second):
	}
	return err
}
