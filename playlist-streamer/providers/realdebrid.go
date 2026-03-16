package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const rdBaseURL = "https://api.real-debrid.com/rest/1.0"

// RealDebrid resolves magnet links / torrent URLs to direct HTTP streams
// via the Real-Debrid API. ffmpeg can read the resulting URLs directly,
// so no download-then-stream is needed.
type RealDebrid struct {
	apiToken   string
	httpClient *http.Client
	pollInterval time.Duration
	maxWait      time.Duration
}

func NewRealDebrid() *RealDebrid {
	return &RealDebrid{
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		pollInterval: 5 * time.Second,
		maxWait:      30 * time.Minute,
	}
}

func (rd *RealDebrid) SetAPIToken(token string) { rd.apiToken = token }

func (rd *RealDebrid) Name() string        { return "realdebrid" }
func (rd *RealDebrid) StreamViaPipe() bool  { return false }

func (rd *RealDebrid) ValidateURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("empty URL")
	}
	if strings.HasPrefix(rawURL, "magnet:") {
		return rawURL, nil
	}
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		return rawURL, nil
	}
	return "", fmt.Errorf("realdebrid: unsupported URL scheme: %s", rawURL)
}

// GetStreamURL takes a magnet link (or hoster link) and returns a direct
// HTTP URL that ffmpeg can stream from.
//
// For magnets the flow is:
//  1. POST /torrents/addMagnet → torrent ID
//  2. POST /torrents/selectFiles/{id} (files=all)
//  3. Poll GET /torrents/info/{id} until status=downloaded
//  4. POST /unrestrict/link on the largest file's link → direct URL
func (rd *RealDebrid) GetStreamURL(rawURL string) (string, error) {
	if rd.apiToken == "" {
		return "", fmt.Errorf("realdebrid: api_token not configured")
	}

	if strings.HasPrefix(rawURL, "magnet:") {
		return rd.resolveMagnet(rawURL)
	}
	// Regular hoster link — just unrestrict directly
	return rd.unrestrictLink(rawURL)
}

// ── magnet resolution ───────────────────────────────────────────────

type addMagnetResp struct {
	ID  string `json:"id"`
	URI string `json:"uri"`
}

type torrentInfo struct {
	ID       string       `json:"id"`
	Filename string       `json:"filename"`
	Status   string       `json:"status"`
	Progress float64      `json:"progress"`
	Links    []string     `json:"links"`
	Files    []torrentFile `json:"files"`
}

type torrentFile struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Selected int    `json:"selected"`
}

func (rd *RealDebrid) resolveMagnet(magnet string) (string, error) {
	// 1. Add magnet
	resp, err := rd.post("/torrents/addMagnet", url.Values{"magnet": {magnet}})
	if err != nil {
		return "", fmt.Errorf("addMagnet: %w", err)
	}
	var addResp addMagnetResp
	if err := json.Unmarshal(resp, &addResp); err != nil {
		return "", fmt.Errorf("addMagnet parse: %w (body: %s)", err, string(resp))
	}
	torrentID := addResp.ID
	if torrentID == "" {
		return "", fmt.Errorf("addMagnet returned empty id (body: %s)", string(resp))
	}
	log.Printf("[realdebrid] Torrent added: %s", torrentID)

	// 2. Select all files
	_, err = rd.post("/torrents/selectFiles/"+torrentID, url.Values{"files": {"all"}})
	if err != nil {
		return "", fmt.Errorf("selectFiles: %w", err)
	}
	log.Printf("[realdebrid] Files selected, waiting for download...")

	// 3. Poll until downloaded
	info, err := rd.pollTorrent(torrentID)
	if err != nil {
		return "", err
	}

	if len(info.Links) == 0 {
		return "", fmt.Errorf("torrent has no links after download")
	}

	// Pick the link corresponding to the largest file
	link := rd.pickBestLink(info)
	log.Printf("[realdebrid] Unrestricting link for %s...", info.Filename)

	return rd.unrestrictLink(link)
}

func (rd *RealDebrid) pollTorrent(id string) (*torrentInfo, error) {
	deadline := time.Now().Add(rd.maxWait)
	for {
		body, err := rd.get("/torrents/info/" + id)
		if err != nil {
			return nil, fmt.Errorf("torrents/info: %w", err)
		}
		var info torrentInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return nil, fmt.Errorf("parse torrent info: %w", err)
		}

		switch info.Status {
		case "downloaded":
			log.Printf("[realdebrid] Torrent ready: %s", info.Filename)
			return &info, nil
		case "magnet_error", "error", "virus", "dead":
			return nil, fmt.Errorf("torrent failed with status: %s", info.Status)
		default:
			// waiting_files_selection, magnet_conversion, queued, downloading, uploading, compressing
			log.Printf("[realdebrid] Status: %s (%.0f%%), waiting...", info.Status, info.Progress)
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("torrent did not finish within %v (status: %s, progress: %.0f%%)", rd.maxWait, info.Status, info.Progress)
		}
		time.Sleep(rd.pollInterval)
	}
}

// pickBestLink returns the link associated with the largest file.
// Real-Debrid returns links[] in the same order as selected files.
func (rd *RealDebrid) pickBestLink(info *torrentInfo) string {
	if len(info.Links) == 1 {
		return info.Links[0]
	}

	// Build list of selected files sorted by size
	type selected struct {
		idx   int
		bytes int64
	}
	var sel []selected
	for _, f := range info.Files {
		if f.Selected == 1 {
			sel = append(sel, selected{len(sel), f.Bytes})
		}
	}

	bestIdx := 0
	var bestSize int64
	for _, s := range sel {
		if s.bytes > bestSize {
			bestSize = s.bytes
			bestIdx = s.idx
		}
	}

	if bestIdx < len(info.Links) {
		return info.Links[bestIdx]
	}
	return info.Links[0]
}

// ── unrestrict ──────────────────────────────────────────────────────

type unrestrictResp struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Filesize int64  `json:"filesize"`
	Link     string `json:"link"`
	Download string `json:"download"`
	Streamable int  `json:"streamable"`
}

func (rd *RealDebrid) unrestrictLink(link string) (string, error) {
	resp, err := rd.post("/unrestrict/link", url.Values{"link": {link}})
	if err != nil {
		return "", fmt.Errorf("unrestrict/link: %w", err)
	}
	var ur unrestrictResp
	if err := json.Unmarshal(resp, &ur); err != nil {
		return "", fmt.Errorf("unrestrict parse: %w (body: %s)", err, string(resp))
	}
	if ur.Download == "" {
		return "", fmt.Errorf("unrestrict returned empty download URL (body: %s)", string(resp))
	}
	log.Printf("[realdebrid] Unrestricted: %s (%s, %d MB)", ur.Filename, ur.MimeType, ur.Filesize/(1024*1024))
	return ur.Download, nil
}

// ── HTTP helpers ────────────────────────────────────────────────────

func (rd *RealDebrid) get(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", rdBaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rd.apiToken)
	resp, err := rd.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func (rd *RealDebrid) post(path string, form url.Values) ([]byte, error) {
	var bodyReader io.Reader
	contentType := "application/x-www-form-urlencoded"
	if form != nil {
		bodyReader = bytes.NewBufferString(form.Encode())
	}
	req, err := http.NewRequest("POST", rdBaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rd.apiToken)
	if form != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := rd.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
