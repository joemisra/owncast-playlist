package providers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPlexBrowseAndResolve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("X-Plex-Token") != "secret" {
			http.Error(w, "missing token", 401)
			return
		}
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`<MediaContainer><Directory key="1" title="Movies" type="movie"/><Directory key="2" title="TV" type="show"/></MediaContainer>`))
		case "/library/sections/2/all":
			if r.URL.Query().Get("type") != "4" {
				t.Error("TV request did not select episodes")
			}
			w.Write([]byte(`<MediaContainer><Video ratingKey="42" title="Pilot" grandparentTitle="Example" parentTitle="Season 1" type="episode" parentIndex="1" index="1"><Media><Part file="/tv/Example/Season 01/Example - S01E01 - Pilot.mkv"/></Media></Video></MediaContainer>`))
		case "/library/metadata/42":
			w.Write([]byte(`<MediaContainer><Video><Media><Part key="/library/parts/99/file.mkv"/></Media></Video></MediaContainer>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := NewPlex([]PlexServer{{Name: "shared", BaseURL: server.URL, Token: "secret"}})
	libraries, err := p.ListLibraries()
	if err != nil || len(libraries) != 2 {
		t.Fatalf("libraries: %#v, %v", libraries, err)
	}
	items, err := p.ListItems("shared", "2", "show")
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Title, "S01E01") {
		t.Fatalf("items: %#v, %v", items, err)
	}
	if items[0].ShowTitle != "Example" || items[0].SeasonTitle != "Season 1" || items[0].Season != 1 || items[0].Episode != 1 {
		t.Fatalf("missing TV hierarchy: %#v", items[0])
	}
	if items[0].FileName != "Example - S01E01 - Pilot.mkv" {
		t.Fatalf("bad filename: %q", items[0].FileName)
	}
	stream, err := p.GetStreamURL(items[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(stream)
	if u.Path != "/library/parts/99/file.mkv" || u.Query().Get("X-Plex-Token") != "secret" {
		t.Fatalf("bad stream URL: %s", stream)
	}
}

func TestPlexMovieFileMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections/1/all" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`<MediaContainer><Video ratingKey="7" title="Example Movie" type="movie" year="2025"><Media><Part file="Z:\Movies\Example Movie (2025)\Example Movie.mkv"/></Media></Video></MediaContainer>`))
	}))
	defer server.Close()

	p := NewPlex([]PlexServer{{Name: "home", BaseURL: server.URL, Token: "secret"}})
	items, err := p.ListItems("home", "1", "movie")
	if err != nil || len(items) != 1 {
		t.Fatalf("items: %#v, %v", items, err)
	}
	if items[0].Title != "Example Movie" || items[0].Year != 2025 || items[0].FileName != "Example Movie.mkv" {
		t.Fatalf("missing movie hierarchy: %#v", items[0])
	}
}

func TestPlexURLValidation(t *testing.T) {
	p := NewPlex([]PlexServer{{Name: "home", BaseURL: "http://plex", Token: "x"}})
	if got, err := p.ValidateURL("plex://home/123"); err != nil || got != "plex://home/123" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := p.ValidateURL("plex://other/123"); err == nil {
		t.Fatal("expected unknown server error")
	}
}
