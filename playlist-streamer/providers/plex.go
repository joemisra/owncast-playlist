package providers

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type PlexServer struct {
	Name, BaseURL, Token string
}

type Plex struct {
	servers map[string]PlexServer
	client  *http.Client
}

type PlexLibrary struct {
	Server string `json:"server"`
	Key    string `json:"key"`
	Title  string `json:"title"`
	Type   string `json:"type"`
}

type PlexItem struct {
	Server      string `json:"server"`
	RatingKey   string `json:"ratingKey"`
	Title       string `json:"title"`
	Year        int    `json:"year,omitempty"`
	Type        string `json:"type"`
	ShowTitle   string `json:"showTitle,omitempty"`
	SeasonTitle string `json:"seasonTitle,omitempty"`
	Season      int    `json:"season,omitempty"`
	Episode     int    `json:"episode,omitempty"`
	FileName    string `json:"fileName,omitempty"`
	URL         string `json:"url"`
}

func NewPlex(servers []PlexServer) *Plex {
	p := &Plex{servers: make(map[string]PlexServer), client: &http.Client{Timeout: 30 * time.Second}}
	for _, s := range servers {
		s.Name = strings.TrimSpace(s.Name)
		s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if s.Name != "" && s.BaseURL != "" {
			p.servers[s.Name] = s
		}
	}
	return p
}

func (p *Plex) Name() string        { return "plex" }
func (p *Plex) StreamViaPipe() bool { return false }

func (p *Plex) ValidateURL(raw string) (string, error) {
	server, key, err := parsePlexURL(raw)
	if err != nil {
		return "", err
	}
	if _, ok := p.servers[server]; !ok {
		return "", fmt.Errorf("plex: unknown server %q", server)
	}
	return "plex://" + url.PathEscape(server) + "/" + url.PathEscape(key), nil
}

func parsePlexURL(raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "plex" || u.Host == "" {
		return "", "", fmt.Errorf("plex: expected plex://server/rating-key")
	}
	key, err := url.PathUnescape(strings.TrimPrefix(u.EscapedPath(), "/"))
	if err != nil || key == "" || strings.Contains(key, "/") {
		return "", "", fmt.Errorf("plex: invalid rating key")
	}
	return u.Host, key, nil
}

func (p *Plex) GetStreamURL(raw string) (string, error) {
	serverName, key, err := parsePlexURL(raw)
	if err != nil {
		return "", err
	}
	s, ok := p.servers[serverName]
	if !ok {
		return "", fmt.Errorf("plex: unknown server %q", serverName)
	}
	var container struct {
		Videos []struct {
			Media []struct {
				Parts []struct {
					Key string `xml:"key,attr"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err := p.getXML(s, "/library/metadata/"+url.PathEscape(key), nil, &container); err != nil {
		return "", err
	}
	if len(container.Videos) == 0 || len(container.Videos[0].Media) == 0 || len(container.Videos[0].Media[0].Parts) == 0 {
		return "", fmt.Errorf("plex: item %s has no playable media", key)
	}
	return p.authURL(s, container.Videos[0].Media[0].Parts[0].Key), nil
}

func (p *Plex) ListLibraries() ([]PlexLibrary, error) {
	var out []PlexLibrary
	for _, s := range p.servers {
		var container struct {
			Directories []struct {
				Key   string `xml:"key,attr"`
				Title string `xml:"title,attr"`
				Type  string `xml:"type,attr"`
			} `xml:"Directory"`
		}
		if err := p.getXML(s, "/library/sections", nil, &container); err != nil {
			return nil, fmt.Errorf("plex %s: %w", s.Name, err)
		}
		for _, d := range container.Directories {
			if d.Type == "movie" || d.Type == "show" {
				out = append(out, PlexLibrary{Server: s.Name, Key: d.Key, Title: d.Title, Type: d.Type})
			}
		}
	}
	return out, nil
}

func (p *Plex) ListItems(serverName, section, libraryType string) ([]PlexItem, error) {
	s, ok := p.servers[serverName]
	if !ok {
		return nil, fmt.Errorf("plex: unknown server %q", serverName)
	}
	if section == "" || strings.Contains(section, "/") {
		return nil, fmt.Errorf("plex: invalid library key")
	}
	params := url.Values{"X-Plex-Container-Start": {"0"}, "X-Plex-Container-Size": {"500"}}
	if libraryType == "show" {
		params.Set("type", "4") // episodes, rather than non-playable show directories
	}
	var container struct {
		Videos []struct {
			RatingKey   string `xml:"ratingKey,attr"`
			Title       string `xml:"title,attr"`
			Grandparent string `xml:"grandparentTitle,attr"`
			Parent      string `xml:"parentTitle,attr"`
			Type        string `xml:"type,attr"`
			Year        int    `xml:"year,attr"`
			Season      int    `xml:"parentIndex,attr"`
			Episode     int    `xml:"index,attr"`
			Media       []struct {
				Parts []struct {
					File string `xml:"file,attr"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err := p.getXML(s, "/library/sections/"+url.PathEscape(section)+"/all", params, &container); err != nil {
		return nil, err
	}
	out := make([]PlexItem, 0, len(container.Videos))
	for _, v := range container.Videos {
		if v.RatingKey != "" {
			title := v.Title
			if v.Type == "episode" && v.Grandparent != "" {
				if v.Season > 0 {
					title = fmt.Sprintf("%s · S%02dE%02d · %s", v.Grandparent, v.Season, v.Episode, v.Title)
				} else {
					title = fmt.Sprintf("%s · E%02d · %s", v.Grandparent, v.Episode, v.Title)
				}
			}
			fileName := ""
			if len(v.Media) > 0 && len(v.Media[0].Parts) > 0 {
				fileName = plexFileName(v.Media[0].Parts[0].File)
			}
			out = append(out, PlexItem{
				Server: serverName, RatingKey: v.RatingKey, Title: title, Year: v.Year,
				Type: v.Type, ShowTitle: v.Grandparent, SeasonTitle: v.Parent,
				Season: v.Season, Episode: v.Episode, FileName: fileName,
				URL: "plex://" + url.PathEscape(serverName) + "/" + url.PathEscape(v.RatingKey),
			})
		}
	}
	return out, nil
}

func plexFileName(file string) string {
	// Plex may report paths from Unix, Windows, or network-mounted libraries.
	file = strings.ReplaceAll(strings.TrimSpace(file), `\`, "/")
	if file == "" {
		return ""
	}
	return path.Base(file)
}

func (p *Plex) getXML(s PlexServer, path string, params url.Values, dst any) error {
	if s.Token == "" {
		return fmt.Errorf("token not configured")
	}
	u := strings.TrimRight(s.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	if len(params) > 0 {
		parsed, _ := url.Parse(u)
		q := parsed.Query()
		for k, values := range params {
			for _, value := range values {
				q.Add(k, value)
			}
		}
		parsed.RawQuery = q.Encode()
		u = parsed.String()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/xml")
	// Keep credentials out of the URL. Go includes request URLs in connection
	// errors, and those errors are surfaced to the dashboard.
	req.Header.Set("X-Plex-Token", s.Token)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Do not reflect upstream bodies into the manager UI. A Plex proxy or
		// error page could include credentials or other private data.
		return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	if err := xml.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (p *Plex) authURL(s PlexServer, path string) string {
	u, _ := url.Parse(s.BaseURL + "/" + strings.TrimLeft(path, "/"))
	q := u.Query()
	q.Set("X-Plex-Token", s.Token)
	u.RawQuery = q.Encode()
	return u.String()
}
