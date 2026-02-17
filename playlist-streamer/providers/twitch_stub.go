package providers

// TwitchStub is a placeholder for future Twitch VOD streaming support.
// Twitch VODs require Twitch API/Helix and optional auth. See Twitch API docs.
type TwitchStub struct{}

// NewTwitchStub creates the Twitch stub provider.
func NewTwitchStub() *TwitchStub {
	return &TwitchStub{}
}

// Name returns "twitch".
func (t *TwitchStub) Name() string {
	return "twitch"
}

// StreamViaPipe returns false.
func (t *TwitchStub) StreamViaPipe() bool {
	return false
}

// ValidateURL would validate Twitch VOD URLs. Not implemented.
func (t *TwitchStub) ValidateURL(url string) (string, error) {
	return "", ErrNotImplemented
}

// GetStreamURL would resolve Twitch VOD to a direct stream URL. Not implemented.
func (t *TwitchStub) GetStreamURL(videoID string) (string, error) {
	return "", ErrNotImplemented
}
