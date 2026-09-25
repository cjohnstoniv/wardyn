// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hostEqual compares two hostnames case-insensitively, ignoring a trailing dot
// and surrounding whitespace (DNS names are case-insensitive; "host." == "host").
func hostEqual(a, b string) bool {
	norm := func(h string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")) }
	return norm(a) == norm(b)
}

// subscriptionOAuthSecret is the SENTINEL secret name (not a stored secret): an
// api_key injection grant carrying it resolves to the operator's LIVE Anthropic
// subscription OAuth access token (from the resident ~/.claude via
// Config.SubscriptionToken) instead of a value in the secret store. This lets
// subscription runs be credentialed proxy-side exactly like api-key runs. In the
// default inject-on mode the sandbox's staged .credentials.json carries only inert
// sentinel tokens (stage-claude-creds.sh replaces BOTH the durable refresh token and
// the short-lived access token) so no usable credential is resident; the live token
// exists only in proxy memory. Defined canonically in internal/types so recordmode +
// UI agree on the name.
const subscriptionOAuthSecret = types.SubscriptionOAuthSecret

// subscriptionInjectionHost is the vendor-default host the subscription/
// managed OAuth sentinels may target. They resolve to a LIVE Anthropic OAuth
// access token, which has exactly one correct destination; injecting it
// anywhere else would exfiltrate a long-lived operator credential. See
// subscriptionInjectionHostAllowed for the one other host this may widen to.
const subscriptionInjectionHost = "api.anthropic.com"

// subscriptionInjectionHostAllowed reports whether host is a permitted target
// for the subscription/managed OAuth sentinel: the vendor default, or the
// operator-configured Anthropic gateway (anthropicGatewayHost) — taken from
// CONFIGURATION, never from host itself or anything else a grant/request
// supplies. That distinction is the security property: an authored/inline/
// recorded grant can never widen its own destination by naming a host that
// happens to match; only the operator's own boot-time
// WARDYN_ANTHROPIC_BASE_URL can add the second host this accepts.
func (s *Server) subscriptionInjectionHostAllowed(host string) bool {
	if hostEqual(host, subscriptionInjectionHost) {
		return true
	}
	if h := s.anthropicGatewayHost(); h != "" && hostEqual(host, h) {
		return true
	}
	return false
}

// subscriptionInjectionHostDesc is the human-readable list of hosts
// subscriptionInjectionHostAllowed accepts, for refusal text.
func (s *Server) subscriptionInjectionHostDesc() string {
	if h := s.anthropicGatewayHost(); h != "" {
		return subscriptionInjectionHost + " or the configured gateway " + h
	}
	return subscriptionInjectionHost
}

// oauthProviderForSentinel maps a grant's secret name to the OAuth token
// provider that resolves it, if it is one of the two Anthropic OAuth sentinels.
// Both resolve through the same subscription.Provider interface and the same
// injection sink (host-pinned, forced Bearer, masked) — they differ only in
// SOURCE (resident host token vs Wardyn-managed captured token), which the
// returned source label records in the audit. isSentinel=false for any ordinary
// stored-secret grant (handled by the generic path below).
func (s *Server) oauthProviderForSentinel(secretName string) (provider subscription.Provider, source string, isSentinel bool) {
	switch secretName {
	case subscriptionOAuthSecret:
		return s.cfg.SubscriptionToken, "subscription", true
	case types.ManagedOAuthSecret:
		return s.cfg.ManagedToken, "managed", true
	default:
		return nil, "", false
	}
}

// injectionResponse carries the FORMATTED secret value the proxy injects. It is
// an ALIAS, not a copy: the wardyn-proxy decodes this exact struct, so the wire
// contract is single-sourced and the compiler (not a parity test) enforces that
// both sides agree — see types.ResolvedInjection for why.
type injectionResponse = types.ResolvedInjection

// withStoreRow adds the row a SiteAudited read reported — its store, ref and
// owner (secretstore.Row.AuditData) — to a site's secret.read data. A read that
// found no row adds nothing.
func withStoreRow(data map[string]any, row *secretstore.Row) map[string]any {
	for k, v := range row.AuditData() {
		data[k] = v
	}
	return data
}

// handleInternalInjection resolves an api_key grant to its injectable header
// value for the run's wardyn-proxy sidecar (startup mint).
//
// Security: this endpoint returns a secret VALUE to a run-token-authed caller.
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
		s.writeMintError(w, r, err)
		return
	}
	if minted.Kind != types.GrantAPIKey || minted.Injection == nil {
		// Only api_key grants resolve here: github/cloud credentials are
		// minted via the regular mint endpoint and never resolved to raw
		// platform secrets.
		writeError(w, http.StatusUnprocessableEntity, "grant is not an api_key injection grant")
		return
	}

	// PER-PERSON CLAUDE SUBSCRIPTION: a wardyn-provider-<uid>-oauth sentinel
	// resolves to the run owner's own Claude sign-in for the provider the run
	// chose — see resolveProviderSubscriptionInjection. The posture 403 below is
	// keyed by sentinel name: it guards the two legacy SHARED sentinels only,
	// so a per-owner name never reaches it (MP-4b retires it).
	if s.resolveProviderSubscriptionInjection(w, r, claims, minted, grantID) {
		return
	}

	// SUBSCRIPTION / MANAGED path: the sentinel secret name resolves to a LIVE
	// Anthropic OAuth access token (the resident host token, or the Wardyn-managed
	// captured setup-token) rather than a stored secret. The token lives only in
	// proxy memory (masked from streams); the sandbox holds an inert sentinel.
	if provider, source, isSentinel := s.oauthProviderForSentinel(minted.Injection.SecretName); isSentinel {
		sentinel := minted.Injection.SecretName
		// Host pin: fail closed unless the grant targets Anthropic. An
		// authored/inline/recorded grant could set this sentinel's host to any
		// egress-allowlisted host; because we force Authorization: Bearer <token>
		// below with a LIVE OAuth token, a non-Anthropic host would exfiltrate that
		// token (in cleartext on a plain-HTTP allowlist entry). This is the single
		// sink chokepoint that protects every caller; the policy validator rejects a
		// mis-authored host earlier as defense-in-depth.
		if !s.subscriptionInjectionHostAllowed(minted.Injection.Host) {
			s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
				"secret.read", sentinel, "failure",
				mustJSON(map[string]any{"reason": "oauth-host-not-anthropic", "host": minted.Injection.Host, "grant_id": grantID, "source": source})))
			writeError(w, http.StatusForbidden, "the subscription OAuth token may only be injected to "+s.subscriptionInjectionHostDesc())
			return
		}
		// Posture pin: refuse to resolve a SHARED subscription credential unless this
		// deployment is single-user (subscriptionInjectPosture, cmd/wardynd). Sits on
		// the same chokepoint as the host pin above and for the same reason — this is
		// the ONE place every producer of a sentinel grant converges. Dispatch-time
		// gating alone would miss four of them: a stored policy naming the sentinel,
		// a member-supplied integration_id, a Record Mode profile that captured the
		// grant, and the managed lane's default fallback. It also misses the drift
		// case entirely: a grant authored while the daemon was single-user is
		// re-resolved by the still-running proxy after a restart into a multi-user
		// posture, because resident tokens carry an expiry and the injector refreshes.
		//
		// MUST precede provider.Current() below: Current() shells out to the resident
		// `claude` and ROTATES the operator's own ~/.claude/.credentials.json. Refusing
		// after it would still mutate their personal credential on behalf of a run we
		// just decided was not entitled to it.
		if !s.cfg.SubscriptionPostureOK {
			s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
				"secret.read", sentinel, "failure",
				mustJSON(map[string]any{"reason": "shared-subscription-posture", "grant_id": grantID, "source": source, "detail": s.cfg.SubscriptionPostureReason})))
			writeError(w, http.StatusForbidden, "shared subscription credentials are not available in this deployment: "+s.cfg.SubscriptionPostureReason)
			return
		}
		if provider == nil {
			s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
				"secret.read", sentinel, "failure",
				mustJSON(map[string]any{"reason": "no-oauth-provider", "grant_id": grantID, "source": source})))
			writeError(w, http.StatusFailedDependency, source+" token provider is not configured")
			return
		}
		tok, terr := provider.Current(r.Context())
		if terr != nil {
			// Fail closed: never inject an expired/absent token.
			s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
				"secret.read", sentinel, "failure",
				mustJSON(map[string]any{"reason": "resolve-failed", "grant_id": grantID, "source": source})))
			writeError(w, http.StatusFailedDependency, "resolve "+source+" token: "+terr.Error())
			return
		}
		// The OAuth token has exactly ONE correct wire shape: Authorization: Bearer
		// <token>. Force it here regardless of the grant's authored header/format — a
		// recorded profile can carry a crossed-wire sentinel grant (x-api-key/%s)
		// that would otherwise inject the token in the wrong header. Host stays the
		// grant's (api.anthropic.com).
		const subHeader, subFormat = "Authorization", "Bearer %s"
		formatted := formatInjectionValue(subFormat, []byte(tok.Value))
		if s.cfg.MaskRegistry != nil {
			s.cfg.MaskRegistry.Add(claims.RunID, []byte(tok.Value))
			if formatted != tok.Value {
				s.cfg.MaskRegistry.Add(claims.RunID, []byte(formatted))
			}
		}
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", sentinel, "success",
			mustJSON(map[string]any{"purpose": "proxy-injection-subscription", "grant_id": grantID, "jti": minted.JTI, "source": source})))
		resp := injectionResponse{
			Host:   minted.Injection.Host,
			Header: subHeader,
			Value:  formatted,
			JTI:    minted.JTI,
		}
		// Only advertise an expiry when the provider has a machine-readable one
		// (resident subscription token). The managed setup-token has none (zero
		// time), so the proxy treats it as static — no re-resolve churn.
		if !tok.ExpiresAt.IsZero() {
			resp.ExpiresAt = tok.ExpiresAt.UnixMilli()
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Captured AWS SSO path (PHASE B): the third sentinel, and the only one that
	// can answer 423. It re-derives the credential's scope from the live roster,
	// requires equality with the grant's dispatch-time snapshot, and either
	// injects the live session or raises a human-visible sign-in request. It is
	// a separate file because it is a different KIND of resolve — see
	// resolveAWSSSOInjection.
	if s.resolveAWSSSOInjection(w, r, claims, minted, grantID) {
		return
	}
	// PER-PERSON AZURE DEVOPS (the fourth sentinel): the run owner's own
	// captured Entra sign-in, redeemed for an access token, pinned to the
	// dispatch-time snapshot and the organisation's own hosts — see
	// resolveADOInjection.
	if s.resolveADOInjection(w, r, claims, minted, grantID) {
		return
	}

	// Defense-in-depth at the SINK: never resolve a sink-reserved secret (signing/
	// session key or a resident AWS Bedrock SigV4 credential) into an injectable header
	// VALUE, regardless of how the grant was authored (stored policy, inline,
	// auto-mint, or a row written before the write-time guard existed). Leaking
	// the identity-signing or session-HMAC key as a Bearer header to ANY host
	// would let a policy forge run identities or session cookies. Fail closed +
	// audit BEFORE reading the value. This is the single chokepoint that protects
	// every current and future caller; the policy validator rejects it earlier.
	if sinkReservedSecret(minted.Injection.SecretName) {
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", minted.Injection.SecretName, "failure",
			mustJSON(map[string]any{"reason": "reserved-secret-name", "grant_id": grantID})))
		writeError(w, http.StatusForbidden, "secret name is reserved for platform internals")
		return
	}

	// Defense-in-depth at the SINK, same posture as the reserved-name check
	// above: the header NAME is operator-authored (an integration's credential
	// header, an api_key grant scope in a stored/inline/recorded policy) and the
	// proxy writes it verbatim onto a forwarded request, so a name carrying CR/LF
	// is a header-splitting shape. Every write boundary rejects it first
	// (validateIntegrationWrite, validateEligibleGrant); this is the one
	// chokepoint that also covers a row written before those existed.
	if !egress.ValidHeaderName(minted.Injection.Header) {
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", minted.Injection.SecretName, "failure",
			mustJSON(map[string]any{"reason": "invalid-header-name", "grant_id": grantID})))
		writeError(w, http.StatusForbidden, "injection header name is not a valid HTTP header")
		return
	}

	// bedrock-api-key is the ONE stored name whose namespace the ROSTER decides,
	// so the owner-fallback read below never resolves it: it resolves from the
	// namespace dispatch recorded on its own grant, or not at all — see
	// resolveBedrockBearerInjection.
	if s.resolveBedrockBearerInjection(w, r, claims, minted, grantID) {
		return
	}

	// The run's OWN identity (claims.Sub) resolves it: the run's creator's own
	// row wins, falling back to the operator's — never another member's, even
	// one named by hand in this run's inline policy (structural: For(owner)
	// never resolves a different owner's row). For an operator-created run
	// claims.Sub is the operator's own identity string (admin-token,
	// local:<op>, or an admin's OIDC sub) — never "" (the identity minter
	// refuses an empty subject) — and secretOwnerFromRequest stamps an
	// operator's writes under "" only, so no row ever exists under those
	// strings and the lookup falls back to the operator row: today's single
	// namespace, unchanged, for every pre-0.7 deployment.
	rctx, row := secretstore.SiteAudited(r.Context())
	secret, err := s.cfg.Secrets.For(claims.Sub).Get(rctx, minted.Injection.SecretName)
	if err != nil {
		// Fail closed; the proxy refuses to start without its injections. The
		// reason tells a store outage from a credential that is gone or refused.
		reason := "refused"
		switch {
		case errors.Is(err, secretstore.ErrUnavailable):
			reason = "store-unavailable"
		case errors.Is(err, secretstore.ErrNotFound):
			reason = "not-found"
		}
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", minted.Injection.SecretName, "failure",
			mustJSON(withStoreRow(map[string]any{"purpose": "proxy-injection", "reason": reason, "grant_id": grantID, "owner": claims.Sub}, row))))
		if reason == "store-unavailable" {
			// Transient: the organisation's store did not answer. A distinct
			// status, so it is never mistaken for a credential that is gone.
			writeError(w, http.StatusServiceUnavailable,
				"Wardyn couldn't reach the service that holds this run's credential, so it couldn't unlock it. Nothing was substituted. Try again in a moment.")
			return
		}
		msg := "secret " + minted.Injection.SecretName + " is not in the store (set it with `wardyn secret set`)"
		if reason == "refused" { // the row exists: re-setting it would overwrite what an operator may need to inspect
			msg = "secret " + minted.Injection.SecretName + " exists but could not be used: the store refused it (its value is gone, or bound to another row). Nothing was substituted; ask an admin to check it."
		}
		writeError(w, http.StatusFailedDependency, msg)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", minted.Injection.SecretName, "success",
		mustJSON(withStoreRow(map[string]any{"purpose": "proxy-injection", "grant_id": grantID, "jti": minted.JTI, "owner": claims.Sub}, row))))

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
