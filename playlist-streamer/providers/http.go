package providers

import (
	"fmt"
	"net/url"
	"strings"
)

// HTTP provider for direct HTTP/HTTPS URLs to movies or other streamable content.
// ffmpeg can read these URLs directly—no piping or API resolution needed.
type HTTP struct{}

// NewHTTP creates an HTTP provider.
func NewHTTP() *HTTP {
	return &HTTP{}
}

func (h *HTTP) Name() string        { return "http" }
func (h *HTTP) StreamViaPipe() bool { return false }

func (h *HTTP) ValidateURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("empty URL")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("http: URL must start with http:// or https://")
	}
	_, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("http: invalid URL: %w", err)
	}
	return rawURL, nil
}

// GetStreamURL returns the URL as-is; ffmpeg can stream it directly.
func (h *HTTP) GetStreamURL(rawURL string) (string, error) {
	if _, err := h.ValidateURL(rawURL); err != nil {
		return "", err
	}
	return rawURL, nil
}
