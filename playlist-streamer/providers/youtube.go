package providers

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// YouTube provider uses yt-dlp to resolve YouTube URLs to direct stream URLs.
type YouTube struct {
	ytdlpPath          string
	cookiesFile         string
	cookiesFromBrowser  string
}

// NewYouTube creates a YouTube provider. ytdlpPath is the path to yt-dlp
// (or youtube-dl); empty means "yt-dlp" from PATH.
func NewYouTube() *YouTube {
	return &YouTube{ytdlpPath: "yt-dlp"}
}

// SetYtdlpPath sets a custom path for yt-dlp.
func (y *YouTube) SetYtdlpPath(path string) {
	y.ytdlpPath = path
}

// SetCookiesFile sets path to Netscape-format cookies.txt (export from logged-in browser).
func (y *YouTube) SetCookiesFile(path string) {
	y.cookiesFile = path
}

// SetCookiesFromBrowser sets browser name (chrome, firefox, etc.) to use its cookies.
func (y *YouTube) SetCookiesFromBrowser(browser string) {
	y.cookiesFromBrowser = browser
}

// GetCookiesFile returns the cookies file path.
func (y *YouTube) GetCookiesFile() string { return y.cookiesFile }

// GetCookiesFromBrowser returns the cookies-from-browser value.
func (y *YouTube) GetCookiesFromBrowser() string { return y.cookiesFromBrowser }

// Name returns "youtube".
func (y *YouTube) Name() string {
	return "youtube"
}

// StreamViaPipe returns true - YouTube direct URLs require cookies, so we must
// pipe yt-dlp output to ffmpeg instead of passing the URL.
func (y *YouTube) StreamViaPipe() bool {
	return true
}

// ValidateURL normalizes YouTube URLs and extracts video ID.
func (y *YouTube) ValidateURL(url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", fmt.Errorf("empty youtube url")
	}
	// Accept full URLs or bare video IDs
	if id := extractYouTubeVideoID(url); id != "" {
		return id, nil
	}
	// Maybe it's already a video ID (11 chars)
	if len(url) == 11 && regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(url) {
		return url, nil
	}
	return "", fmt.Errorf("invalid youtube url or video id: %s", url)
}

// GetStreamURL runs yt-dlp -g to get a direct URL ffmpeg can read.
func (y *YouTube) GetStreamURL(videoID string) (string, error) {
	url := videoID
	if !strings.Contains(videoID, "://") {
		url = "https://www.youtube.com/watch?v=" + videoID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"-f", "best[ext=mp4]/best"}
	if y.cookiesFile != "" {
		args = append(args, "--cookies", y.cookiesFile)
	}
	if y.cookiesFromBrowser != "" {
		args = append(args, "--cookies-from-browser", y.cookiesFromBrowser)
	}
	args = append(args, "-g", url)
	cmd := exec.CommandContext(ctx, y.ytdlpPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("yt-dlp: %w (stderr: %s)", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("yt-dlp: %w", err)
	}
	streamURL := strings.TrimSpace(string(out))
	if streamURL == "" {
		return "", fmt.Errorf("yt-dlp returned empty url")
	}
	return streamURL, nil
}

func extractYouTubeVideoID(s string) string {
	// youtube.com/watch?v=VIDEO_ID
	if idx := strings.Index(s, "youtube.com/watch?v="); idx >= 0 {
		rest := s[idx+20:]
		if end := strings.IndexAny(rest, "&?"); end >= 0 {
			rest = rest[:end]
		}
		if len(rest) >= 11 {
			return rest[:11]
		}
		return rest
	}
	// youtu.be/VIDEO_ID
	if idx := strings.Index(s, "youtu.be/"); idx >= 0 {
		rest := s[idx+9:]
		if end := strings.IndexAny(rest, "?&"); end >= 0 {
			rest = rest[:end]
		}
		if len(rest) >= 11 {
			return rest[:11]
		}
		return rest
	}
	return ""
}
