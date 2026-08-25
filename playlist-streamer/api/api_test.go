package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"playlist-streamer/config"
)

func TestRequireSessionUsesForwardedPrefixForLogin(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	handler := s.requireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler was called without a session")
	}))
	req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/stream")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if got := response.Header().Get("Location"); got != "/stream/login.html" {
		t.Fatalf("Location = %q, want %q", got, "/stream/login.html")
	}
}

func TestPrefixedPathRejectsUnsafePrefix(t *testing.T) {
	for _, prefix := range []string{"https://example.com", "/../outside", "/stream?next=outside", `/stream\\outside`} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		req.Header.Set("X-Forwarded-Prefix", prefix)
		if got := prefixedPath(req, "/login.html"); got != "/login.html" {
			t.Errorf("prefix %q produced %q", prefix, got)
		}
	}
}
