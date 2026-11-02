// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
)

// gatewayJWKSRoute is the key set's path. Experimental until 0.11.
const gatewayJWKSRoute = "/.well-known/wardyn-experimental/gateway-jwks.json"

// handleGatewayJWKS serves the public keys an AI gateway verifies a run identity
// token with. Anonymous by design: the gateway holds no Wardyn credential, and
// the set holds public keys only. mountUI registers it.
//
// Config.GatewayIdentity returns the signer: (nil, nil) means no key exists yet,
// create makes one, and an error means a stored key could not be read, which is
// never minted over.
//
// With identity off it answers exactly as a path no route holds, so an install
// that never turned it on shows nothing new (a gateway with jwtAuth therefore
// cannot start against it). Off means no enabled provider has identity on
// (gatewayIdentityOn), asked before the signer, which a replica keeps for its life:
// a key row left after the last provider turned off does not publish.
//
// The max-age is 60 seconds, the shortest a gateway on agentgateway v1.7.0
// honours (read from its source: it defaults to 15 minutes and floors a
// Cache-Control max-age at 60 seconds). A gateway accepts a retired key until it
// refetches, and until it refetches it answers 401 to a token from a new key, so
// both windows are worth one small anonymous request a minute.
func (s *Server) handleGatewayJWKS(w http.ResponseWriter, r *http.Request) {
	on, err := s.gatewayIdentityOn(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error"})
		return
	}
	var signer *gatewayidentity.Signer
	if on && s.cfg.GatewayIdentity != nil {
		if signer, err = s.cfg.GatewayIdentity(r.Context(), false); err != nil {
			slog.Error("gateway identity: load the signing key for the JWKS", slog.Any("err", err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error"})
			return
		}
	}
	if signer == nil {
		// What mountUI's GET /* gives a path naming no file, or the router's 404
		// without a console. Other methods never reach here: chi's 405 with a
		// console, mountUI's NotFound without one.
		if s.cfg.UIDir == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		s.serveIndex(w, r, filepath.Join(s.cfg.UIDir, "index.html"))
		return
	}
	body, err := signer.JWKS()
	if err != nil {
		slog.Error("gateway identity: encode the JWKS", slog.Any("err", err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_, _ = w.Write(body)
}
