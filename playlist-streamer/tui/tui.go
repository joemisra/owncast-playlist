package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"playlist-streamer/playlist"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"
)

var knownProviders = []string{"youtube", "realdebrid", "kick", "twitch"}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170"))

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("212")).
			Bold(true)

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	okStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("82"))

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("99")).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(lipgloss.Color("240"))

	providerColors = map[string]lipgloss.Color{
		"youtube":    "196",
		"realdebrid": "214",
		"kick":       "82",
		"twitch":     "135",
	}
)

func providerTag(name string) string {
	c, ok := providerColors[name]
	if !ok {
		c = "252"
	}
	return lipgloss.NewStyle().Foreground(c).Render(fmt.Sprintf("[%s]", name))
}

type viewState int

const (
	viewFiles viewState = iota
	viewPlaylist
	viewAddVideo
)

type model struct {
	playlistDir string
	files       []string
	fileCursor  int

	currentFile *playlist.PlaylistFile
	currentPath string

	videoCursor    int
	providerCursor int
	currentView    viewState
	urlInput       textinput.Model

	dirty       bool
	statusMsg   string
	statusIsErr bool
	width       int
	height      int
}

func newModel(dir string) model {
	ti := textinput.New()
	ti.Placeholder = "https://youtube.com/watch?v=... or magnet:?xt=..."
	ti.CharLimit = 500
	ti.Width = 60

	m := model{
		playlistDir: dir,
		urlInput:    ti,
	}
	m.loadFiles()
	return m
}

func (m *model) loadFiles() {
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

func (m *model) setStatus(msg string) { m.statusMsg = msg; m.statusIsErr = false }
func (m *model) setError(msg string)  { m.statusMsg = msg; m.statusIsErr = true }

func (m *model) currentPlaylist() *playlist.Playlist {
	if m.currentFile == nil || len(m.currentFile.Playlists) == 0 {
		return nil
	}
	return &m.currentFile.Playlists[0]
}

func (m *model) savePlaylist() error {
	if m.currentFile == nil {
		return fmt.Errorf("no file loaded")
	}
	data, err := yaml.Marshal(m.currentFile)
	if err != nil {
		return err
	}
	return os.WriteFile(m.currentPath, data, 0644)
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	switch m.currentView {
	case viewFiles:
		return m.updateFiles(msg)
	case viewPlaylist:
		return m.updatePlaylist(msg)
	case viewAddVideo:
		return m.updateAddVideo(msg)
	}
	return m, nil
}

func (m model) updateFiles(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		m.currentFile = pf
		m.currentPath = path
		m.currentView = viewPlaylist
		m.videoCursor = 0
		m.dirty = false
		m.setStatus(fmt.Sprintf("Loaded %s", m.files[m.fileCursor]))
	case "r":
		m.loadFiles()
		m.setStatus("Refreshed")
	case "q":
		return m, tea.Quit
	}
	return m, nil
}

func (m model) updatePlaylist(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
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
		if pl != nil && m.videoCursor > 0 {
			pl.Videos[m.videoCursor], pl.Videos[m.videoCursor-1] = pl.Videos[m.videoCursor-1], pl.Videos[m.videoCursor]
			m.videoCursor--
			m.dirty = true
		}
	case "J":
		if pl != nil && m.videoCursor < len(pl.Videos)-1 {
			pl.Videos[m.videoCursor], pl.Videos[m.videoCursor+1] = pl.Videos[m.videoCursor+1], pl.Videos[m.videoCursor]
			m.videoCursor++
			m.dirty = true
		}
	case "a", "n":
		m.currentView = viewAddVideo
		m.urlInput.SetValue("")
		m.urlInput.Focus()
		m.providerCursor = 0
		m.statusMsg = ""
		return m, textinput.Blink
	case "d", "x":
		if pl != nil && len(pl.Videos) > 0 {
			pl.Videos = append(pl.Videos[:m.videoCursor], pl.Videos[m.videoCursor+1:]...)
			if m.videoCursor >= len(pl.Videos) && m.videoCursor > 0 {
				m.videoCursor--
			}
			m.dirty = true
			m.setStatus("Deleted")
		}
	case "s":
		if err := m.savePlaylist(); err != nil {
			m.setError(fmt.Sprintf("save failed: %v", err))
		} else {
			m.dirty = false
			m.setStatus("Saved!")
		}
	case "q":
		if m.dirty {
			m.setError("Unsaved changes! 's' to save, 'Q' to discard and quit")
			break
		}
		m.currentView = viewFiles
		m.currentFile = nil
		m.statusMsg = ""
	case "Q":
		m.currentView = viewFiles
		m.currentFile = nil
		m.dirty = false
		m.statusMsg = ""
	case "esc":
		if m.dirty {
			m.setError("Unsaved changes! 's' to save, 'Q' to discard")
			break
		}
		m.currentView = viewFiles
		m.currentFile = nil
		m.statusMsg = ""
	}
	return m, nil
}

func (m model) updateAddVideo(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if ok {
		switch key.Type {
		case tea.KeyEsc:
			m.currentView = viewPlaylist
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
			pl := m.currentPlaylist()
			if pl != nil {
				pl.Videos = append(pl.Videos, playlist.VideoEntry{
					URL:      url,
					Provider: knownProviders[m.providerCursor],
				})
				m.videoCursor = len(pl.Videos) - 1
				m.dirty = true
				m.setStatus(fmt.Sprintf("Added %s video", knownProviders[m.providerCursor]))
			}
			m.currentView = viewPlaylist
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.urlInput, cmd = m.urlInput.Update(msg)
	return m, cmd
}

// --- Views ---

func (m model) View() string {
	var b strings.Builder

	switch m.currentView {
	case viewFiles:
		m.viewFiles(&b)
	case viewPlaylist:
		m.viewPlaylist(&b)
	case viewAddVideo:
		m.viewAddVideo(&b)
	}

	if m.statusMsg != "" {
		b.WriteString("\n")
		if m.statusIsErr {
			b.WriteString(errorStyle.Render("  " + m.statusMsg))
		} else {
			b.WriteString(okStyle.Render("  " + m.statusMsg))
		}
	}

	return b.String()
}

func (m model) viewFiles(b *strings.Builder) {
	b.WriteString(headerStyle.Render("  Playlist Editor"))
	b.WriteString("\n\n")

	if len(m.files) == 0 {
		b.WriteString(dimStyle.Render("  No playlist files found in " + m.playlistDir))
		b.WriteString("\n")
	} else {
		for i, f := range m.files {
			if i == m.fileCursor {
				b.WriteString(selectedStyle.Render("  ▸ " + f))
			} else {
				b.WriteString(normalStyle.Render("    " + f))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑↓/jk navigate • enter open • r refresh • q quit"))
}

func (m model) viewPlaylist(b *strings.Builder) {
	pl := m.currentPlaylist()
	filename := filepath.Base(m.currentPath)
	mark := ""
	if m.dirty {
		mark = " *"
	}

	b.WriteString(headerStyle.Render(fmt.Sprintf("  %s%s", filename, mark)))
	b.WriteString("\n\n")

	if pl == nil {
		b.WriteString(dimStyle.Render("  Empty file — press 'a' to add a video"))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("  a add • esc back"))
		return
	}

	b.WriteString(titleStyle.Render(fmt.Sprintf("  %s", pl.Name)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("  (%d videos)", len(pl.Videos))))
	if pl.Schedule != "" {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  cron: %s", pl.Schedule)))
	}
	b.WriteString("\n\n")

	if len(pl.Videos) == 0 {
		b.WriteString(dimStyle.Render("  (empty — press 'a' to add a video)"))
		b.WriteString("\n")
	} else {
		maxURL := m.width - 30
		for i, v := range pl.Videos {
			num := fmt.Sprintf("%2d.", i+1)
			tag := providerTag(v.Provider)
			url := truncate(v.URL, maxURL)

			if i == m.videoCursor {
				b.WriteString(selectedStyle.Render(fmt.Sprintf("  ▸ %s ", num)))
				b.WriteString(tag)
				b.WriteString(selectedStyle.Render(" " + url))
			} else {
				b.WriteString(normalStyle.Render(fmt.Sprintf("    %s ", num)))
				b.WriteString(tag)
				b.WriteString(normalStyle.Render(" " + url))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑↓/jk navigate • a add • d delete • J/K reorder • s save • q/esc back"))
}

func (m model) viewAddVideo(b *strings.Builder) {
	b.WriteString(headerStyle.Render("  Add Video"))
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
	b.WriteString(helpStyle.Render("  tab cycle provider • enter confirm • esc cancel"))
}

func truncate(s string, max int) string {
	if max < 20 {
		max = 20
	}
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// Run launches the TUI playlist editor.
func Run(playlistDir string) error {
	p := tea.NewProgram(newModel(playlistDir), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
