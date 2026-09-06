package providers

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSMBResolvesConfiguredShareAndListsMedia(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Show Name", "Season 01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, "Episode 01.mkv")
	if err := os.WriteFile(media, make([]byte, 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := NewSMB([]SMBShare{{Name: "TV", Path: root}})
	got, err := provider.GetStreamURL("smb://tv/Show%20Name/Season%2001/Episode%2001.mkv")
	if err != nil || got != media {
		t.Fatalf("GetStreamURL() = %q, %v; want %q", got, err, media)
	}
	items, err := provider.List("tv", "Show Name/Season 01")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Episode 01.mkv" || items[0].URL == "" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestSMBRejectsTraversalAndUnknownShares(t *testing.T) {
	provider := NewSMB([]SMBShare{{Name: "movies", Path: t.TempDir()}})
	for _, raw := range []string{
		"smb://movies/../secret.mkv",
		"smb://movies/%2e%2e/secret.mkv",
		"smb://other/file.mkv",
	} {
		if _, err := provider.ValidateURL(raw); err == nil {
			t.Errorf("ValidateURL(%q) unexpectedly succeeded", raw)
		}
	}
}
