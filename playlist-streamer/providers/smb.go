package providers

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SMBShare maps a short playlist-safe name to an operating-system mount.
// Credentials never enter playlist-streamer; the mount owns authentication.
type SMBShare struct {
	Name string
	Path string
}

type SMBShareInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

type SMBItem struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	URL       string `json:"url,omitempty"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size,omitempty"`
}

// SMB resolves logical smb://share/path URLs into paths beneath configured,
// read-only mounts. It does not mount shares or handle credentials.
type SMB struct {
	shares map[string]SMBShare
}

func NewSMB(shares []SMBShare) *SMB {
	s := &SMB{shares: make(map[string]SMBShare)}
	for _, share := range shares {
		name := strings.TrimSpace(share.Name)
		root := filepath.Clean(strings.TrimSpace(share.Path))
		if name == "" || root == "." || !filepath.IsAbs(root) {
			continue
		}
		share.Name = name
		share.Path = root
		s.shares[strings.ToLower(name)] = share
	}
	return s
}

func (s *SMB) Name() string        { return "smb" }
func (s *SMB) StreamViaPipe() bool { return false }

func (s *SMB) ValidateURL(raw string) (string, error) {
	share, relative, err := parseSMBURL(raw)
	if err != nil {
		return "", err
	}
	configured, ok := s.shares[strings.ToLower(share)]
	if !ok {
		return "", fmt.Errorf("smb: unknown share %q", share)
	}
	if _, err := withinRoot(configured.Path, relative); err != nil {
		return "", err
	}
	return smbURL(configured.Name, relative), nil
}

func (s *SMB) GetStreamURL(raw string) (string, error) {
	share, relative, err := parseSMBURL(raw)
	if err != nil {
		return "", err
	}
	configured, ok := s.shares[strings.ToLower(share)]
	if !ok {
		return "", fmt.Errorf("smb: unknown share %q", share)
	}
	resolved, err := withinRoot(configured.Path, relative)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("smb: media unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 1000 {
		return "", fmt.Errorf("smb: media is not a usable file")
	}
	return resolved, nil
}

func (s *SMB) ListShares() []SMBShareInfo {
	out := make([]SMBShareInfo, 0, len(s.shares))
	for _, share := range s.shares {
		info, err := os.Stat(share.Path)
		out = append(out, SMBShareInfo{Name: share.Name, Available: err == nil && info.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (s *SMB) List(shareName, relative string) ([]SMBItem, error) {
	share, ok := s.shares[strings.ToLower(strings.TrimSpace(shareName))]
	if !ok {
		return nil, fmt.Errorf("smb: unknown share %q", shareName)
	}
	resolved, err := withinRoot(share.Path, relative)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("smb: browse failed: %w", err)
	}
	cleanRelative := cleanSMBPath(relative)
	out := make([]SMBItem, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		itemRelative := path.Join(cleanRelative, entry.Name())
		if entry.IsDir() {
			out = append(out, SMBItem{Name: entry.Name(), Path: itemRelative, Directory: true})
			continue
		}
		if !isMediaFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, SMBItem{
			Name: entry.Name(), Path: itemRelative, URL: smbURL(share.Name, itemRelative), Size: info.Size(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Directory != out[j].Directory {
			return out[i].Directory
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func parseSMBURL(raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.ToLower(u.Scheme) != "smb" || u.Host == "" {
		return "", "", fmt.Errorf("smb: expected smb://share/path")
	}
	relative, err := url.PathUnescape(strings.TrimPrefix(u.EscapedPath(), "/"))
	if err != nil || relative == "" || strings.ContainsRune(relative, '\x00') {
		return "", "", fmt.Errorf("smb: invalid media path")
	}
	clean := cleanSMBPath(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return "", "", fmt.Errorf("smb: invalid media path")
	}
	return u.Host, clean, nil
}

func cleanSMBPath(relative string) string {
	return path.Clean(strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(relative), `\`, "/"), "/"))
}

func withinRoot(root, relative string) (string, error) {
	clean := cleanSMBPath(relative)
	if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("smb: path leaves configured share")
	}
	resolved := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("smb: path leaves configured share")
	}
	return resolved, nil
}

func smbURL(share, relative string) string {
	return (&url.URL{Scheme: "smb", Host: share, Path: "/" + cleanSMBPath(relative)}).String()
}

func isMediaFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".m4v", ".mov", ".avi", ".ts", ".webm", ".flv":
		return true
	default:
		return false
	}
}
