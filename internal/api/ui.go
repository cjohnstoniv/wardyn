// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// fallbackStatusPage is served at "/" when no UIDir is configured. It is a tiny
// self-contained page (no build step) confirming the control plane is up.
const fallbackStatusPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>Wardyn control plane</title>
<style>body{font-family:ui-monospace,Menlo,monospace;background:#0d1117;color:#c9d1d9;margin:0;padding:3rem;}
h1{color:#58a6ff;font-size:1.4rem;}code{color:#7ee787;}a{color:#58a6ff;}</style></head>
<body><h1>wardyn control plane</h1>
<p>The API is running. No UI bundle is configured (set <code>-ui-dir</code>).</p>
<p>Health: <a href="/healthz">/healthz</a> &middot; API base: <code>/api/v1</code></p>
</body></html>`

// mountUI wires the web UI. With a UIDir it serves the SPA (index.html fallback
// for client-side routes); without one it serves the embedded status page at /.
// The /api and /healthz routes are already registered and take precedence.
func (s *Server) mountUI(r chi.Router) {
	if s.cfg.UIDir == "" {
		r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(fallbackStatusPage))
		})
		return
	}
	dir := s.cfg.UIDir
	fs := http.FileServer(http.Dir(dir))
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		// Serve the file if it exists; otherwise fall back to index.html so the
		// SPA router can handle client-side paths (history API mode).
		clean := filepath.Clean(strings.TrimPrefix(req.URL.Path, "/"))
		full := filepath.Join(dir, clean)
		if clean != "." && withinDir(dir, full) {
			if info, err := os.Stat(full); err == nil && !info.IsDir() {
				// Hashed build artifacts under /assets/ are content-addressed and
				// safe to cache forever; everything else (favicon, manifest) revalidates.
				if strings.HasPrefix(req.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				fs.ServeHTTP(w, req)
				return
			}
		}
		// index.html must always revalidate so a redeploy's new hashed bundle is
		// picked up instead of a stale cached shell.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, req, filepath.Join(dir, "index.html"))
	})
}

// withinDir guards against path traversal escaping the UI directory.
func withinDir(dir, full string) bool {
	rel, err := filepath.Rel(dir, full)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}
