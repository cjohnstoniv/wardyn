// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// reservedSecret reports whether name is a platform-internal / managed-credential
// key that the generic secrets API must not touch and the injection sink must not
// resolve as a raw value. It covers the static reservedSecretNames set PLUS the
// managed-harness token-blob PATTERN (wardyn-harness-<provider>-oauth) — so a
// FUTURE provider row in agentHarnessLogin (e.g. codex) is sealed automatically,
// closing the "add a provider, forget to reserve its blob" landmine even though
// its name is generated dynamically by harnessCredSecretName. Use this at every
// reserved-name guard instead of a bare map lookup.
func reservedSecret(name string) bool {
	if reservedSecretNames[name] {
		return true
	}
	return strings.HasPrefix(name, "wardyn-harness-") && strings.HasSuffix(name, "-oauth")
}

// sinkReservedSecret is the reserved-name guard at the credential SINKS — the
// api_key injection resolver (handleInternalInjection), the git_pat/ssh_key
// broker mints, and the policy write-time checks that mirror them. It is
// reservedSecret() PLUS the three RESIDENT AWS SigV4 credential names that
// resolveBedrockAuth reads DIRECTLY from the store to sign Bedrock requests
// (aws-access-key-id / aws-secret-access-key / aws-session-token). Those never
// flow through a grant on the legitimate Bedrock path, so an api_key/git_pat/
// ssh_key grant naming one is only ever an attempt to exfiltrate the operator's
// long-lived AWS secret key to an allowlisted host (as a Bearer header or git
// password) — reject it at every sink. bedrock-api-key is deliberately EXCLUDED:
// the Bedrock BEARER path authors a host-pinned api_key grant that legitimately
// resolves it through the injection sink (see runs.go), so reserving it here
// would break that path.
func sinkReservedSecret(name string) bool {
	return reservedSecret(name) ||
		name == bedrockAccessKeyIDSecret ||
		name == bedrockSecretAccessKeySecret ||
		name == bedrockSessionTokenSecret
}

// secretsAPIReserved is the reserved-name guard for the GENERIC secrets API
// (Put/Delete/List). It is reservedSecret() PLUS the two Anthropic OAuth
// injection SENTINELS (types.SubscriptionOAuthSecret / types.ManagedOAuthSecret).
// A sentinel is name-privileged — an api_key grant carrying it resolves at the
// injection sink to a LIVE OAuth token via oauthProviderForSentinel, IGNORING any
// stored value — so letting an operator Put a value under that name (silently
// shadowed) or listing it is confusing at best and hides that the name is
// credential-privileged at worst. Reserved from the generic API ONLY, never at
// the sinks: a subscription/managed policy legitimately names the sentinel in an
// api_key grant, which validateInlineSecretRefs and the injection sink allow via
// the provider switch (oauthProviderForSentinel), which runs AFTER this guard.
func secretsAPIReserved(name string) bool {
	return reservedSecret(name) || name == types.SubscriptionOAuthSecret || name == types.ManagedOAuthSecret
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
	if secretsAPIReserved(name) {
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
		if secretsAPIReserved(n) {
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
