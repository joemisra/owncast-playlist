package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"playlist-streamer/config"
)

const minimumCachedMediaSize = 100_000

type temporaryPlexCacheError struct {
	err error
}

func (e temporaryPlexCacheError) Error() string { return e.err.Error() }
func (e temporaryPlexCacheError) Unwrap() error { return e.err }

func isTemporaryPlexCacheError(err error) bool {
	var temporary temporaryPlexCacheError
	return errors.As(err, &temporary)
}

// plexCache stores complete Plex media files under opaque names. Direct Plex
// URLs (and their credentials) are never used as filenames or log labels.
type plexCache struct {
	dir          string
	maxBytes     int64
	minFreeBytes int64
	client       *http.Client
}

func newPlexCache(cfg config.StreamerConfig) *plexCache {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &plexCache{
		dir:          filepath.Clean(cfg.PlexCacheDir),
		maxBytes:     cfg.PlexCacheMaxGB * 1024 * 1024 * 1024,
		minFreeBytes: cfg.PlexCacheMinFreeGB * 1024 * 1024 * 1024,
		client:       &http.Client{Transport: transport},
	}
}

func (c *plexCache) cachedPath(cacheKey, sourceURL string) (string, bool) {
	path := c.path(cacheKey, sourceURL)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < minimumCachedMediaSize {
		return path, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return path, true
}

// fetch downloads one Plex media file. A partial download remains on disk and
// is resumed with an HTTP Range request on the next attempt.
func (c *plexCache) fetch(ctx context.Context, cacheKey, sourceURL string, protected map[string]bool, progress func(received, total int64)) (string, error) {
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return "", fmt.Errorf("create Plex cache: %w", err)
	}
	finalPath, hit := c.cachedPath(cacheKey, sourceURL)
	protected[finalPath] = true
	if hit {
		return finalPath, nil
	}

	partialPath := finalPath + ".partial"
	protected[partialPath] = true
	offset := int64(0)
	if info, err := os.Stat(partialPath); err == nil && info.Mode().IsRegular() {
		offset = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", fmt.Errorf("create Plex cache request")
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// net/http errors include the request URL. Do not return it because the
		// Plex token may be present in the query string.
		return "", temporaryPlexCacheError{err: fmt.Errorf("Plex cache request failed")}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if offset == 0 {
			return "", fmt.Errorf("Plex cache returned an unexpected partial response")
		}
	case http.StatusOK:
		// Some servers ignore Range. Restart the partial file rather than
		// appending a second complete copy.
		offset = 0
	case http.StatusRequestedRangeNotSatisfiable:
		total := contentRangeTotal(resp.Header.Get("Content-Range"))
		if total > 0 && offset == total {
			if err := os.Rename(partialPath, finalPath); err != nil {
				return "", fmt.Errorf("complete Plex cache file: %w", err)
			}
			return finalPath, nil
		}
		return "", fmt.Errorf("Plex cache resume was rejected (HTTP %d)", resp.StatusCode)
	default:
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return "", temporaryPlexCacheError{err: fmt.Errorf("Plex cache returned HTTP %d", resp.StatusCode)}
		}
		return "", fmt.Errorf("Plex cache returned HTTP %d", resp.StatusCode)
	}

	totalSize := responseTotalSize(resp, offset)
	if totalSize < minimumCachedMediaSize {
		return "", fmt.Errorf("Plex media size is unavailable or too small")
	}
	remaining := totalSize - offset
	if remaining < 0 {
		return "", fmt.Errorf("Plex cache partial file is larger than the source")
	}
	if err := c.ensureCapacity(remaining, protected); err != nil {
		return "", err
	}
	if progress != nil {
		progress(offset, totalSize)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}
	file, err := os.OpenFile(partialPath, flags, 0o640)
	if err != nil {
		return "", fmt.Errorf("open Plex cache partial file: %w", err)
	}
	destination := io.Writer(file)
	if progress != nil {
		destination = &cacheProgressWriter{writer: file, current: offset, total: totalSize, progress: progress}
	}
	written, copyErr := io.CopyBuffer(destination, resp.Body, make([]byte, 1024*1024))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", temporaryPlexCacheError{err: fmt.Errorf("write Plex cache: %w", copyErr)}
	}
	if syncErr != nil {
		return "", fmt.Errorf("sync Plex cache: %w", syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close Plex cache: %w", closeErr)
	}
	if offset+written != totalSize {
		return "", temporaryPlexCacheError{err: fmt.Errorf("Plex cache download incomplete: received %d of %d bytes", offset+written, totalSize)}
	}
	if err := os.Rename(partialPath, finalPath); err != nil {
		return "", fmt.Errorf("complete Plex cache file: %w", err)
	}
	return finalPath, nil
}

type cacheProgressWriter struct {
	writer   io.Writer
	current  int64
	total    int64
	progress func(received, total int64)
}

func (w *cacheProgressWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.current += int64(n)
	w.progress(w.current, w.total)
	return n, err
}

func (c *plexCache) path(cacheKey, sourceURL string) string {
	sum := sha256.Sum256([]byte(cacheKey))
	name := hex.EncodeToString(sum[:16]) + safeMediaExtension(sourceURL)
	return filepath.Join(c.dir, name)
}

func safeMediaExtension(sourceURL string) string {
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return ".media"
	}
	ext := strings.ToLower(filepath.Ext(parsed.Path))
	switch ext {
	case ".mkv", ".mp4", ".m4v", ".mov", ".avi", ".ts", ".webm":
		return ext
	default:
		return ".media"
	}
}

func responseTotalSize(resp *http.Response, offset int64) int64 {
	if resp.StatusCode == http.StatusPartialContent {
		if total := contentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
			return total
		}
	}
	if resp.ContentLength > 0 {
		return offset + resp.ContentLength
	}
	return 0
}

func contentRangeTotal(value string) int64 {
	slash := strings.LastIndex(value, "/")
	if slash < 0 || slash == len(value)-1 || value[slash+1:] == "*" {
		return 0
	}
	total, _ := strconv.ParseInt(value[slash+1:], 10, 64)
	return total
}

type cacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

func (c *plexCache) ensureCapacity(incoming int64, protected map[string]bool) error {
	entries, used, err := c.entries()
	if err != nil {
		return err
	}
	free, err := diskFreeBytes(c.dir)
	if err != nil {
		return fmt.Errorf("inspect Plex cache disk: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modTime.Before(entries[j].modTime) })
	needsSpace := func() bool {
		overCacheLimit := c.maxBytes > 0 && used+incoming > c.maxBytes
		belowDiskReserve := c.minFreeBytes > 0 && free < incoming+c.minFreeBytes
		return overCacheLimit || belowDiskReserve
	}
	for _, entry := range entries {
		if !needsSpace() {
			break
		}
		if protected[entry.path] {
			continue
		}
		if err := os.Remove(entry.path); err != nil {
			return fmt.Errorf("evict Plex cache entry: %w", err)
		}
		used -= entry.size
		free += entry.size
	}
	if c.maxBytes > 0 && used+incoming > c.maxBytes {
		return fmt.Errorf("Plex playlist exceeds the %d GB cache limit", c.maxBytes/(1024*1024*1024))
	}
	if c.minFreeBytes > 0 && free < incoming+c.minFreeBytes {
		return fmt.Errorf("Plex cache would leave less than %d GB free", c.minFreeBytes/(1024*1024*1024))
	}
	return nil
}

func (c *plexCache) entries() ([]cacheEntry, int64, error) {
	dirEntries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read Plex cache: %w", err)
	}
	entries := make([]cacheEntry, 0, len(dirEntries))
	var used int64
	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		item := cacheEntry{path: filepath.Join(c.dir, entry.Name()), size: info.Size(), modTime: info.ModTime()}
		entries = append(entries, item)
		used += item.size
	}
	return entries, used, nil
}

func diskFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
