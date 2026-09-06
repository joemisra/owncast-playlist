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

func TestRequireTransferAuthAcceptsBearerToken(t *testing.T) {
	s := &Server{cfg: &config.Config{Dashboard: config.DashboardConfig{AdminToken: "transfer-secret"}}}
	called := false
	handler := s.requireTransferAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/cache/queue", nil)
	req.Header.Set("Authorization", "Bearer transfer-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("called=%v status=%d, want true/%d", called, response.Code, http.StatusNoContent)
	}
}

func TestRequireTransferAuthRejectsWrongToken(t *testing.T) {
	s := &Server{cfg: &config.Config{Dashboard: config.DashboardConfig{AdminToken: "transfer-secret"}}}
	handler := s.requireTransferAuth(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler was called with the wrong token")
	})
	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/cache/queue", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusUnauthorized)
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
