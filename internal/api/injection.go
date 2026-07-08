// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// subscriptionOAuthSecret is the SENTINEL secret name (not a stored secret): an
// api_key injection grant carrying it resolves to the operator's LIVE Anthropic
// subscription OAuth access token (from the resident ~/.claude via
// Config.SubscriptionToken) instead of a value in the secret store. This lets
// subscription runs be credentialed proxy-side exactly like api-key runs — the
// sandbox holds only an inert sentinel, never a copy that goes stale. Defined
// canonically in internal/types so recordmode + UI agree on the name.
const subscriptionOAuthSecret = types.SubscriptionOAuthSecret

// injectionResponse carries the FORMATTED secret value the proxy injects.
// ExpiresAt (unix ms, 0 = never) lets the proxy re-resolve a rotating credential
// (the subscription OAuth token) before it lapses; static api-key grants omit it.
type injectionResponse struct {
	Host      string `json:"host"`
	Header    string `json:"header"`
	Value     string `json:"value"`
	JTI       string `json:"jti"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// handleInternalInjection resolves an api_key grant to its injectable header
// value for the run's wardyn-proxy sidecar (startup mint).
//
// SECURITY: this endpoint returns a secret VALUE to a run-token-authed caller.
// That is safe ONLY because the sandbox can never reach it: the sandbox holds
// no run token (the proxy injects it on brokered forwards), and the proxy's
// brokered local routes forward exclusively mint/approvals/recordings — this
// path is structurally unreachable from inside a sandbox. Do not add a
// brokered forward for it. Every resolve emits credential.mint (broker) and
// secret.read audit events.
func (s *Server) handleInternalInjection(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "missing run claims")
		return
	}
	if s.cfg.Broker == nil {
		writeError(w, http.StatusServiceUnavailable, "broker not configured")
		return
	}
	grantID, ok := parseIDParam(w, r, "grantID", "grant")
	if !ok {
		return
	}

	// The broker enforces run ownership, kind dispatch, and audit (jti).
	minted, err := s.cfg.Broker.MintForGrant(r.Context(), claims, grantID)
	if err != nil {
		s.writeMintError(w, err)
		return
	}
	if minted.Kind != types.GrantAPIKey || minted.Injection == nil {
		// Only api_key grants resolve here: github/cloud credentials are
		// minted via the regular mint endpoint and never resolved to raw
		// platform secrets.
		writeError(w, http.StatusUnprocessableEntity, "grant is not an api_key injection grant")
		return
	}

	// SUBSCRIPTION path: the sentinel secret name resolves to the operator's LIVE
	// OAuth access token (host-refreshed) rather than a stored secret. The token
	// lives only in proxy memory (masked from streams); the sandbox holds an inert
	// sentinel. ExpiresAt is returned so the proxy re-resolves before it lapses.
	if minted.Injection.SecretName == subscriptionOAuthSecret {
		if s.cfg.SubscriptionToken == nil {
			s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
				"secret.read", subscriptionOAuthSecret, "failure",
				mustJSON(map[string]any{"reason": "no-subscription-provider", "grant_id": grantID})))
			writeError(w, http.StatusFailedDependency, "subscription token provider is not configured")
			return
		}
		tok, terr := s.cfg.SubscriptionToken.Current(r.Context())
		if terr != nil {
			// Fail closed: never inject an expired/absent subscription token.
			s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
				"secret.read", subscriptionOAuthSecret, "failure",
				mustJSON(map[string]any{"reason": "resolve-failed", "grant_id": grantID})))
			writeError(w, http.StatusFailedDependency, "resolve subscription token: "+terr.Error())
			return
		}
		// The subscription OAuth token has exactly ONE correct wire shape:
		// Authorization: Bearer <token>. Force it here regardless of the grant's
		// authored header/format — a recorded profile can carry a crossed-wire
		// sentinel grant (x-api-key/%s) that would otherwise inject the token in the
		// wrong header. Host stays the grant's (api.anthropic.com).
		const subHeader, subFormat = "Authorization", "Bearer %s"
		formatted := formatInjectionValue(subFormat, []byte(tok.Value))
		if s.cfg.MaskRegistry != nil {
			s.cfg.MaskRegistry.Add(claims.RunID, []byte(tok.Value))
			if formatted != tok.Value {
				s.cfg.MaskRegistry.Add(claims.RunID, []byte(formatted))
			}
		}
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", subscriptionOAuthSecret, "success",
			mustJSON(map[string]any{"purpose": "proxy-injection-subscription", "grant_id": grantID, "jti": minted.JTI})))
		writeJSON(w, http.StatusOK, injectionResponse{
			Host:      minted.Injection.Host,
			Header:    subHeader,
			Value:     formatted,
			JTI:       minted.JTI,
			ExpiresAt: tok.ExpiresAt.UnixMilli(),
		})
		return
	}

	// Defense-in-depth at the SINK: never resolve a reserved platform-internal
	// secret (wardyn-signing-key / wardyn-session-key) into an injectable header
	// VALUE, regardless of how the grant was authored (stored policy, inline,
	// auto-mint, or a row written before the write-time guard existed). Leaking
	// the identity-signing or session-HMAC key as a Bearer header to ANY host
	// would let a policy forge run identities or session cookies. Fail closed +
	// audit BEFORE reading the value. This is the single chokepoint that protects
	// every current and future caller; the policy validator rejects it earlier.
	if reservedSecretNames[minted.Injection.SecretName] {
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", minted.Injection.SecretName, "failure",
			mustJSON(map[string]any{"reason": "reserved-secret-name", "grant_id": grantID})))
		writeError(w, http.StatusForbidden, "secret name is reserved for platform internals")
		return
	}

	secret, err := s.cfg.Secrets.Get(r.Context(), minted.Injection.SecretName)
	if err != nil {
		// Fail closed; the proxy refuses to start without its injections.
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", minted.Injection.SecretName, "failure", nil))
		writeError(w, http.StatusFailedDependency,
			"secret "+minted.Injection.SecretName+" is not in the store (set it with `wardyn secret set`)")
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", minted.Injection.SecretName, "success",
		mustJSON(map[string]any{"purpose": "proxy-injection", "grant_id": grantID, "jti": minted.JTI})))

	formattedValue := formatInjectionValue(minted.Injection.Format, secret)

	// Register the raw secret and formatted value with the mask registry so
	// both forms are masked from PTY/asciicast streams. The formatted value
	// (e.g. "Bearer sk-...") is what the agent might observe in proxy error
	// messages; the raw value covers direct leakage. A nil registry is a no-op.
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, secret)
		if formattedValue != string(secret) {
			s.cfg.MaskRegistry.Add(claims.RunID, []byte(formattedValue))
		}
	}

	writeJSON(w, http.StatusOK, injectionResponse{
		Host:   minted.Injection.Host,
		Header: minted.Injection.Header,
		Value:  formattedValue,
		JTI:    minted.JTI,
	})
}
