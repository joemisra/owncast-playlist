package providers

import "errors"

// ErrNotImplemented is returned when a provider is not yet implemented.
var ErrNotImplemented = errors.New("provider support not yet implemented")

// VideoProvider abstracts video source resolution for different platforms.
type VideoProvider interface {
	// GetStreamURL returns a direct URL ffmpeg can read. For YouTube, these URLs often
	// require cookies and fail with 403 when ffmpeg fetches them directly.
	GetStreamURL(videoID string) (string, error)

	// StreamViaPipe returns true if this provider must pipe through yt-dlp (or similar)
	// instead of passing a URL to ffmpeg. YouTube requires this because direct URLs
	// need cookies that only yt-dlp sends.
	StreamViaPipe() bool

	// ValidateURL normalizes and validates the input URL, returning a canonical ID/URL.
	ValidateURL(url string) (string, error)

	// Name returns the provider name (e.g. "youtube", "kick", "twitch").
	Name() string
}

// Registry holds providers by name.
type Registry struct {
	providers map[string]VideoProvider
}

// NewRegistry creates a registry with built-in providers.
func NewRegistry() *Registry {
	r := &Registry{providers: make(map[string]VideoProvider)}
	r.Register(NewYouTube())
	r.Register(NewKickStub())
	r.Register(NewTwitchStub())
	return r
}

// Register adds a provider.
func (r *Registry) Register(p VideoProvider) {
	r.providers[p.Name()] = p
}

// Get returns the provider for the given name, or nil if not found.
func (r *Registry) Get(name string) VideoProvider {
	return r.providers[name]
}
