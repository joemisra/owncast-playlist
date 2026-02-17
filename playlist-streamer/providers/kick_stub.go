package providers

// KickStub is a placeholder for future Kick.com VOD streaming support.
// Kick VODs would require Kick's API/auth. See Kick API docs for implementation.
type KickStub struct{}

// NewKickStub creates the Kick stub provider.
func NewKickStub() *KickStub {
	return &KickStub{}
}

// Name returns "kick".
func (k *KickStub) Name() string {
	return "kick"
}

// StreamViaPipe returns false.
func (k *KickStub) StreamViaPipe() bool {
	return false
}

// ValidateURL would validate Kick VOD URLs. Not implemented.
func (k *KickStub) ValidateURL(url string) (string, error) {
	return "", ErrNotImplemented
}

// GetStreamURL would resolve Kick VOD to a direct stream URL. Not implemented.
func (k *KickStub) GetStreamURL(videoID string) (string, error) {
	return "", ErrNotImplemented
}
