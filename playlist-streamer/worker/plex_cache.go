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
	"sync"
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

// plexCache is retained as the internal name for the remote-media cache. It
// stores both Plex downloads and files copied from mounted SMB shares under
// opaque names. Source URLs are never used as filenames or log labels.
type plexCache struct {
	dir          string
	maxBytes     int64
	minFreeBytes int64
	client       *http.Client
	capacityMu   sync.Mutex
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

func (c *plexCache) cachedFilePath(cacheKey, sourcePath string, sourceSize int64) (string, bool) {
	path := c.path(cacheKey, sourcePath)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != sourceSize || info.Size() < minimumCachedMediaSize {
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
	if err := c.ensureCapacityLocked(remaining, protected); err != nil {
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

// fetchFile copies one file from a mounted remote share into the local cache.
// Partial files are retained and resumed, so a brief SMB interruption does not
// throw away bytes that already crossed the network.
func (c *plexCache) fetchFile(ctx context.Context, cacheKey, sourcePath string, protected map[string]bool, progress func(received, total int64)) (string, error) {
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return "", fmt.Errorf("create media cache: %w", err)
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if os.IsNotExist(err) || os.IsPermission(err) {
			return "", fmt.Errorf("SMB source is unavailable")
		}
		return "", temporaryPlexCacheError{err: fmt.Errorf("inspect SMB source: %w", err)}
	}
	if !sourceInfo.Mode().IsRegular() || sourceInfo.Size() < minimumCachedMediaSize {
		return "", fmt.Errorf("SMB source is not a usable media file")
	}
	totalSize := sourceInfo.Size()
	finalPath, hit := c.cachedFilePath(cacheKey, sourcePath, totalSize)
	protected[finalPath] = true
	if hit {
		return finalPath, nil
	}
	if info, err := os.Stat(finalPath); err == nil && info.Mode().IsRegular() {
		if err := os.Remove(finalPath); err != nil {
			return "", fmt.Errorf("replace stale media cache entry: %w", err)
		}
	}

	partialPath := finalPath + ".partial"
	protected[partialPath] = true
	offset := int64(0)
	if info, err := os.Stat(partialPath); err == nil && info.Mode().IsRegular() {
		offset = info.Size()
	}
	if offset > totalSize {
		offset = 0
	}
	if offset == totalSize {
		if err := os.Rename(partialPath, finalPath); err != nil {
			return "", fmt.Errorf("complete media cache file: %w", err)
		}
		return finalPath, nil
	}
	if err := c.ensureCapacityLocked(totalSize-offset, protected); err != nil {
		return "", err
	}
	if progress != nil {
		progress(offset, totalSize)
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return "", fmt.Errorf("open SMB source: %w", err)
		}
		return "", temporaryPlexCacheError{err: fmt.Errorf("open SMB source: %w", err)}
	}
	defer source.Close()
	if offset > 0 {
		if _, err := source.Seek(offset, io.SeekStart); err != nil {
			return "", temporaryPlexCacheError{err: fmt.Errorf("resume SMB source: %w", err)}
		}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}
	destinationFile, err := os.OpenFile(partialPath, flags, 0o640)
	if err != nil {
		return "", fmt.Errorf("open media cache partial file: %w", err)
	}
	destination := io.Writer(destinationFile)
	if progress != nil {
		destination = &cacheProgressWriter{writer: destinationFile, current: offset, total: totalSize, progress: progress}
	}
	written, copyErr := io.CopyBuffer(destination, &contextReader{ctx: ctx, reader: source}, make([]byte, 4*1024*1024))
	syncErr := destinationFile.Sync()
	closeErr := destinationFile.Close()
	if copyErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", temporaryPlexCacheError{err: fmt.Errorf("copy SMB media: %w", copyErr)}
	}
	if syncErr != nil {
		return "", fmt.Errorf("sync media cache: %w", syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close media cache: %w", closeErr)
	}
	if offset+written != totalSize {
		return "", temporaryPlexCacheError{err: fmt.Errorf("SMB copy incomplete: received %d of %d bytes", offset+written, totalSize)}
	}
	if err := os.Rename(partialPath, finalPath); err != nil {
		return "", fmt.Errorf("complete media cache file: %w", err)
	}
	return finalPath, nil
}

// receive stores a file pushed to Couch over the authenticated cache API. The
// incoming file is written under a temporary name and becomes visible to the
// streamer only after the complete body is synced and atomically renamed.
func (c *plexCache) receive(ctx context.Context, cacheKey string, source io.Reader, totalSize int64, progress func(received, total int64)) (string, error) {
	if totalSize < minimumCachedMediaSize {
		return "", fmt.Errorf("uploaded media is too small")
	}
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return "", fmt.Errorf("create media cache: %w", err)
	}
	finalPath := c.path(cacheKey, cacheKey)
	if info, err := os.Stat(finalPath); err == nil && info.Mode().IsRegular() && info.Size() == totalSize {
		return finalPath, nil
	}
	protected := map[string]bool{finalPath: true, finalPath + ".incoming": true}
	if err := c.ensureCapacityLocked(totalSize, protected); err != nil {
		return "", err
	}
	incomingPath := finalPath + ".incoming"
	file, err := os.OpenFile(incomingPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", fmt.Errorf("open incoming cache file: %w", err)
	}
	if progress != nil {
		progress(0, totalSize)
	}
	destination := io.Writer(file)
	if progress != nil {
		destination = &cacheProgressWriter{writer: file, total: totalSize, progress: progress}
	}
	written, copyErr := io.CopyBuffer(destination, &contextReader{ctx: ctx, reader: io.LimitReader(source, totalSize+1)}, make([]byte, 4*1024*1024))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(incomingPath)
		return "", fmt.Errorf("receive media upload: %w", copyErr)
	}
	if syncErr != nil {
		_ = os.Remove(incomingPath)
		return "", fmt.Errorf("sync media upload: %w", syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(incomingPath)
		return "", fmt.Errorf("close media upload: %w", closeErr)
	}
	if written != totalSize {
		_ = os.Remove(incomingPath)
		return "", fmt.Errorf("media upload size mismatch: received %d of %d bytes", written, totalSize)
	}
	if err := os.Rename(incomingPath, finalPath); err != nil {
		_ = os.Remove(incomingPath)
		return "", fmt.Errorf("complete media upload: %w", err)
	}
	return finalPath, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(data)
	}
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
		return fmt.Errorf("inspect remote media cache disk: %w", err)
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
			return fmt.Errorf("evict remote media cache entry: %w", err)
		}
		used -= entry.size
		free += entry.size
	}
	if c.maxBytes > 0 && used+incoming > c.maxBytes {
		return fmt.Errorf("remote playlist exceeds the %d GB cache limit", c.maxBytes/(1024*1024*1024))
	}
	if c.minFreeBytes > 0 && free < incoming+c.minFreeBytes {
		return fmt.Errorf("remote media cache would leave less than %d GB free", c.minFreeBytes/(1024*1024*1024))
	}
	return nil
}

func (c *plexCache) ensureCapacityLocked(incoming int64, protected map[string]bool) error {
	c.capacityMu.Lock()
	defer c.capacityMu.Unlock()
	return c.ensureCapacity(incoming, protected)
}

func (c *plexCache) entries() ([]cacheEntry, int64, error) {
	dirEntries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read remote media cache: %w", err)
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
