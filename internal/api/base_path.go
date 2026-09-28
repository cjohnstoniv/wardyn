// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// Handler returns the console listener's handler: the chi router, mounted
// under Config.BasePath when one is set.
func (s *Server) Handler() http.Handler { return underBasePath(s.cfg.BasePath, s.router) }

// InternalHandler returns the chi router at the host root, for the
// proxy-facing TLS listener. Runs dial that listener directly, never through
// the operator's reverse proxy, so WARDYN_BASE_PATH does not apply to it.
func (s *Server) InternalHandler() http.Handler { return s.router }

// underBasePath serves h only under base, with the prefix stripped so every
// route keeps its one registration; a request outside it is a 404. The proxy
// in front forwards the path unchanged. base "" returns h itself.
func underBasePath(base string, h http.Handler) http.Handler {
	if base == "" {
		return h
	}
	strip := http.StripPrefix(base, h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != base && !strings.HasPrefix(r.URL.Path, base+"/") {
			http.NotFound(w, r)
			return
		}
		strip.ServeHTTP(w, r)
	})
}

// cookiePath is the Path of every cookie the console listener sets, and
// consoleCookieName the name it sets and reads one under: both follow the
// sign-in's cookies (oidc.CookieName, oidc.CookiePath), so under secure
// cookies they are __Host- cookies at Path=/.
func (s *Server) cookiePath() string {
	return oidc.CookiePath(s.cfg.OIDCSecureCookies, s.cfg.BasePath)
}

func (s *Server) consoleCookieName(name string) string {
	return oidc.CookieName(s.cfg.OIDCSecureCookies, name)
}

// uiBasePath is the prefix the UI gateway serves under: the base path in
// shared-origin path mode, and nothing in host mode, where every run has an
// origin of its own.
func (s *Server) uiBasePath() string {
	if s.cfg.UIOriginTemplate != "" {
		return ""
	}
	return s.cfg.BasePath
}
