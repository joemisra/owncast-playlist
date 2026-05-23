package providers

import (
	"fmt"
	"os"
	"strings"
)

// Local provider streams a file already on disk via ffmpeg.
type Local struct{}

// NewLocal creates a local file provider.
func NewLocal() *Local { return &Local{} }

func (l *Local) Name() string        { return "local" }
func (l *Local) StreamViaPipe() bool { return false }

func (l *Local) ValidateURL(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("local: empty path")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("local: %s is a directory", path)
	}
	if fi.Size() < 1000 {
		return "", fmt.Errorf("local: %s too small (%d bytes)", path, fi.Size())
	}
	return path, nil
}

// GetStreamURL returns the path as-is; ffmpeg reads the local file directly.
func (l *Local) GetStreamURL(path string) (string, error) {
	return l.ValidateURL(path)
}
