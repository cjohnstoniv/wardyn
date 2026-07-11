// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// secretNameRE constrains secret names to a safe, predictable identifier set.
var secretNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)

// reservedSecretNames are platform-internal keys that must not be overwritten
// or deleted through the API (they would brick identity/session handling), and
// that the injection sink refuses to resolve as a stored value. The Wardyn-
// managed harness OAuth blob lives here too: it is written/deleted ONLY via the
// dedicated setup/harness-credential endpoints and injected ONLY via its
// sentinel (types.ManagedOAuthSecret) through the managed provider — never as a
// raw stored secret, and never listable/clobberable through the generic secrets
// API. Keep in sync with harnessCredSecretName in harnesscred.go.
var reservedSecretNames = map[string]bool{
	"wardyn-signing-key":             true,
	"wardyn-session-key":             true,
	"wardyn-harness-anthropic-oauth": true,
}

// identifierSecretNames hold non-credential IDENTIFIER values (not maskable
// secret material) that are legitimately shorter than secretmask.MinLen — e.g.
// a numeric GitHub App ID. They are exempt from the MinLen gate below; masking
// a public app id would be meaningless, so silently dropping it is not a lie.
var identifierSecretNames = map[string]bool{
	"github-app-id": true,
}

type putSecretRequest struct {
	Value string `json:"value"`
}

// writableSecretName validates a secret name is well-formed and not a
// reserved platform-internal key, writing the appropriate 400/403 and
// returning ok=false on failure. Shared by Put and Delete.
func (s *Server) writableSecretName(w http.ResponseWriter, name string) bool {
	if !secretNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid secret name (lowercase alphanumerics, '.', '_', '-')")
		return false
	}
	if reservedSecretNames[name] {
		writeError(w, http.StatusForbidden, "secret name is reserved for platform internals")
		return false
	}
	return true
}

// handlePutSecret stores (or overwrites) a named secret. The value is write-
// only: no API path ever returns it. Every write is an audit event.
func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !s.writableSecretName(w, name) {
		return
	}
	var body putSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Value == "" {
		writeError(w, http.StatusBadRequest, `body must be {"value":"<non-empty secret>"}`)
		return
	}
	// Fail-CLOSED: reject a secret the masking/scanning layers would SILENTLY drop
	// (secretmask.Add/NewMasker and contentscan.filterCorpus ignore values shorter
	// than secretmask.MinLen). Accepting it would falsely imply it gets masked and
	// scanned; the operator must learn immediately instead. Reserved system keys
	// (signing/session) never reach here — they are set internally, not via this
	// user-facing Put, and are already rejected above. Identifier-style names
	// (e.g. github-app-id, a short numeric App ID) are non-credential and exempt.
	if !identifierSecretNames[name] && len(body.Value) < secretmask.MinLen {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("secret too short: must be at least %d bytes to be masked and scanned", secretmask.MinLen))
		return
	}
	if err := s.cfg.Secrets.Put(r.Context(), name, []byte(body.Value)); err != nil {
		writeError(w, http.StatusInternalServerError, "store secret: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"secret.write", name, "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteSecret removes a named secret. Audited.
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !s.writableSecretName(w, name) {
		return
	}
	if err := s.cfg.Secrets.Delete(r.Context(), name); err != nil {
		writeError(w, http.StatusInternalServerError, "delete secret: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"secret.delete", name, "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// handleListSecrets returns secret NAMES only — never values. Reserved
// platform-internal keys (reservedSecretNames: wardyn-signing-key,
// wardyn-session-key) are EXCLUDED from the listing: they back identity/session
// handling and are not user-managed, so surfacing their names is an unnecessary
// leak (and they are already non-writable/non-deletable via the API).
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	names, err := s.listUserSecretNames(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list secrets: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": names})
}

// listUserSecretNames returns the present secret NAMES (never values) with the
// reserved platform-internal keys (wardyn-signing-key, wardyn-session-key)
// excluded. Shared by handleListSecrets and handleSetupStatus so both surface
// the identical user-managed name set.
func (s *Server) listUserSecretNames(ctx context.Context) ([]string, error) {
	all, err := s.cfg.Secrets.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for _, n := range all {
		if reservedSecretNames[n] {
			continue
		}
		names = append(names, n)
	}
	return names, nil
}

// formatInjectionValue applies an injection rule's Format ("%s"-style) to the
// secret. An empty format means the raw secret value.
func formatInjectionValue(format string, secret []byte) string {
	if format == "" {
		return string(secret)
	}
	return fmt.Sprintf(format, string(secret))
}
