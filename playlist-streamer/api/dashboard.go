package api

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/*
var webFS embed.FS

var dashFS fs.FS

func init() {
	var err error
	dashFS, err = fs.Sub(webFS, "web")
	if err != nil {
		panic("dashboard: embed fs.Sub: " + err.Error())
	}
}

// dashboard wraps the API mux, serving the web dashboard for non-API paths.
func (s *Server) dashboard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Serve files from embedded FS
		name := strings.TrimPrefix(r.URL.Path, "/")

		// Treat empty path or trailing slash as index.html
		if name == "" || strings.HasSuffix(name, "/") {
			name = strings.TrimRight(name, "/") + "/index.html"
			// If it's just "/", name becomes "/index.html" → trim prefix → "index.html"
			name = strings.TrimPrefix(name, "/")
		}

		f, err := dashFS.Open(name)
		if err != nil {
			// Fallback to index.html for SPA routing
			f, err = dashFS.Open("index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
		}
		defer f.Close()

		// Read file contents
		data, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}

		// Set content type
		ctype := "application/octet-stream"
		switch {
		case strings.HasSuffix(name, ".html"):
			ctype = "text/html; charset=utf-8"
		case strings.HasSuffix(name, ".css"):
			ctype = "text/css; charset=utf-8"
		case strings.HasSuffix(name, ".js"):
			ctype = "text/javascript; charset=utf-8"
		case strings.HasSuffix(name, ".svg"):
			ctype = "image/svg+xml"
		case strings.HasSuffix(name, ".png"):
			ctype = "image/png"
		case strings.HasSuffix(name, ".json"):
			ctype = "application/json"
		}
		w.Header().Set("Content-Type", ctype)
		w.Write(data)
	})
}
