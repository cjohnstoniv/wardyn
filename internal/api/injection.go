// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
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

// subscriptionInjectionHost is the vendor host a Claude subscription's runs
// reach when their provider sets no route-through (providerSubscriptionBase).
const subscriptionInjectionHost = "api.anthropic.com"

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

// mintInjectionGrant is handleInternalInjection's prelude, extracted for its
// funlen ratchet: the run's claims, the grant id, and the broker's mint of an
// api_key injection grant. ok=false means the refusal is already written.
func (s *Server) mintInjectionGrant(w http.ResponseWriter, r *http.Request) (*identity.Claims, uuid.UUID, broker.Minted, bool) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return nil, uuid.Nil, broker.Minted{}, false
	}
	if s.cfg.Broker == nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonBrokerNotConfigured, "broker not configured")
		return nil, uuid.Nil, broker.Minted{}, false
	}
	grantID, ok := parseIDParam(w, r, "grantID", "grant")
	if !ok {
		return nil, uuid.Nil, broker.Minted{}, false
	}

	// The broker enforces run ownership, kind dispatch, and audit (jti).
	minted, err := s.cfg.Broker.MintForGrant(r.Context(), claims, grantID)
	if err != nil {
		s.writeMintError(w, r, err)
		return nil, uuid.Nil, broker.Minted{}, false
	}
	if minted.Kind != types.GrantAPIKey || minted.Injection == nil {
		// Only api_key grants resolve here: github/cloud credentials are
		// minted via the regular mint endpoint and never resolved to raw
		// platform secrets.
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonInjectionGrantNotAPIKey, "grant is not an api_key injection grant")
		return nil, uuid.Nil, broker.Minted{}, false
	}
	return claims, grantID, minted, true
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
	claims, grantID, minted, ok := s.mintInjectionGrant(w, r)
	if !ok {
		return
	}

	// PER-PERSON CLAUDE SUBSCRIPTION: a wardyn-provider-<uid>-oauth sentinel
	// resolves to the run owner's own Claude sign-in for the provider the run
	// chose — see resolveProviderSubscriptionInjection.
	if s.resolveProviderSubscriptionInjection(w, r, claims, minted, grantID) {
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
	// resolveADOInjection. A token_mode own_pat grant is the owner's own pasted
	// token instead, and its arm answers first (resolveADOOwnPATInjection).
	if s.resolveADOOwnPATInjection(w, r, claims, minted, grantID) || s.resolveADOInjection(w, r, claims, minted, grantID) {
		return
	}
	// PER-PERSON AZURE FOUNDRY: the run owner's own captured Entra sign-in for the provider the run
	// chose, redeemed for the snapshot's audience and pinned to the provider's own endpoint host — see
	// resolveAzureFoundryInjection. Before the provider-key arm below, which would read a
	// wardyn-provider-<uid>-* name as a key.
	if s.resolveAzureFoundryInjection(w, r, claims, minted, grantID) {
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
			mustJSON(map[string]any{"reason": reasonInjectionReservedSecretName, "grant_id": grantID})))
		writeErrorReason(w, http.StatusForbidden, reasonInjectionReservedSecretName, "secret name is reserved for platform internals")
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
			mustJSON(map[string]any{"reason": reasonInjectionInvalidHeaderName, "grant_id": grantID})))
		writeErrorReason(w, http.StatusForbidden, reasonInjectionInvalidHeaderName, "injection header name is not a valid HTTP header")
		return
	}

	// A person's own model-provider key — for a key, endpoint or Bedrock key
	// provider alike — and bedrock-api-key (the ONE stored name whose namespace
	// the ROSTER decides) never take the owner-fallback read below: each
	// resolves from the namespace dispatch recorded on its own grant, or not at
	// all — see resolveProviderKeyInjection and resolveBedrockBearerInjection.
	if s.resolveProviderKeyInjection(w, r, claims, minted, grantID) ||
		s.resolveBedrockBearerInjection(w, r, claims, minted, grantID) {
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
	// namespace, unchanged, for every pre-0.7 deployment. An owner_only grant
	// never falls back (secretstore.GrantRead).
	//
	// A `shared` grant — the secret an organisation provides for its component
	// — changes WHERE this one read looks and nothing after it: the operator's
	// namespace and only it, whatever the run's owner holds under the same
	// name (injectionGrantRead). The value it finds takes the same format,
	// mask registration, audit row and expiry as every other stored key.
	read, err := s.injectionReadFor(r.Context(), claims, grantID, minted)
	var secret []byte
	gctx, _ := secretstore.GrantRead(r.Context(), read.ownRowOnly)
	rctx, row := secretstore.SiteAudited(gctx)
	if err == nil {
		secret, err = s.cfg.Secrets.For(read.owner).Get(rctx, minted.Injection.SecretName)
	}
	if err != nil {
		// Fail closed; the proxy refuses to start without its injections, and
		// mid-run it acts on the status: see storeReadRefusal.
		status, reason, body := storeReadRefusal(minted.Injection.SecretName, err)
		// The proxy relays this body into the sandbox, so it names the secret
		// only where the name is the run's own to know.
		switch {
		case errors.Is(err, errGrantNotOfRun):
			body = sinkGrantNotOfRun
		case status == http.StatusServiceUnavailable:
		case read.shared:
			body = sinkSharedSecretRefused
		case read.operatorNamespace() && !claims.OperatorOwned:
			body = sinkSecretRefusedUnnamed
			if reason == reasonSinkSecretNotFound {
				body = sinkSecretMissingUnnamed
			}
		}
		slog.WarnContext(r.Context(), "wardynd: a stored credential could not be read for injection",
			slog.String("secret", minted.Injection.SecretName), slog.String("reason", reason), slog.Any("err", err))
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", read.auditTarget(minted.Injection.SecretName, grantID), "failure",
			mustJSON(read.auditData(map[string]any{"purpose": "proxy-injection", "reason": reason, "grant_id": grantID, "owner": claims.Sub}, row))))
		// reason reaches the wire now (#656 slice 3), matching
		// injection_bedrock_bearer.go's identical fix.
		writeErrorReason(w, status, reason, body)
		return
	}
	formattedValue := formatInjectionValue(minted.Injection.Format, secret)

	// Register the raw secret and formatted value with the mask registry so
	// both forms are masked from PTY/asciicast streams. The formatted value
	// (e.g. "Bearer sk-...") is what the agent might observe in proxy error
	// messages; the raw value covers direct leakage. A nil registry is a no-op.
	// A value that is not on record is not handed out.
	if s.refuseUnmasked(w, r, claims, "injection.resolve", secret, []byte(formattedValue)) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", read.auditTarget(minted.Injection.SecretName, grantID), "success",
		mustJSON(read.auditData(map[string]any{"purpose": "proxy-injection", "grant_id": grantID, "jti": minted.JTI, "owner": claims.Sub}, row))))

	writeJSON(w, http.StatusOK, injectionResponse{
		Host:      minted.Injection.Host,
		Header:    minted.Injection.Header,
		Value:     formattedValue,
		JTI:       minted.JTI,
		ExpiresAt: s.storedKeyExpiry(minted),
	})
}

// DRAFT (M2 canon pending) — the sink's answers when a stored secret cannot be
// read and its name is not the run's own to know. The proxy relays the body
// into the sandbox. sinkSharedSecretRefused is for the secret an organisation
// provides for its component. sinkSecretMissingUnnamed and
// sinkSecretRefusedUnnamed are storeReadRefusal's two sentences without the
// name, for a read on a person's run that may land in the operator's
// namespace, where the name could be the operator's: they keep "not in the
// store" apart from "exists but was refused", because setting a refused
// secret again would overwrite what an operator may need to inspect.
// sinkGrantNotOfRun is for a grant the run's own list does not hold.
const (
	sinkSharedSecretRefused  = "The credential your organisation provides for this run could not be read, so nothing was substituted. Ask your admin."
	sinkSecretMissingUnnamed = "A credential this run uses is not in the store, so nothing was substituted. If it is a secret of your own, set it (`wardyn secret set`); otherwise ask your admin."
	sinkSecretRefusedUnnamed = "A credential this run uses exists but could not be used: the store refused it " +
		"(its value is gone or bound to another row, or Wardyn's access to it was revoked). Nothing was substituted; ask an admin to check it."
	sinkGrantNotOfRun = "This credential is not one of this run's grants, so nothing was substituted."
)

// errGrantNotOfRun is a grant the broker minted for that the run's own grant
// list does not hold.
var errGrantNotOfRun = errors.New("the run's grant list does not hold this grant")

// injectionRead is where handleInternalInjection's one stored-key read looks:
// the namespace, whether only that namespace's own row may answer (no operator
// fallback), and whether the grant is a `shared` one.
type injectionRead struct {
	owner      string
	ownRowOnly bool
	shared     bool
	// unknown: the grant's scope could not be read, so whether it is shared
	// is not known and the read is refused.
	unknown bool
}

// operatorNamespace reports whether the read can be answered from the
// operator's namespace: it is the one read, or the fallback is open.
func (rd injectionRead) operatorNamespace() bool {
	return rd.owner == "" || !rd.ownRowOnly
}

// auditTarget is the secret.read row's target. A run's owner may read their
// own run's audit rows, and what an organisation's secret is called is the
// operator's: a `shared` grant's row names the grant, which an operator can
// join to its scope, and never the secret. So does the row of a grant whose
// scope could not be read, which may be a shared one.
func (rd injectionRead) auditTarget(secretName string, grantID uuid.UUID) string {
	if rd.shared || rd.unknown {
		return grantID.String()
	}
	return secretName
}

// auditData completes a secret.read row. A `shared` grant's row is stamped
// with the operator scope and carries nothing of the row it read — the row's
// ref spells the secret's name. Every other row is what it always was.
func (rd injectionRead) auditData(data map[string]any, row *secretstore.Row) map[string]any {
	if rd.shared {
		data["secret_scope"] = "operator"
	}
	if rd.shared || rd.unknown {
		return data
	}
	return withStoreRow(data, row)
}

// injectionReadFor decides where the grant's stored secret is read from. The
// `shared` flag lives in the grant's SCOPE, which the broker's mint does not
// carry, so the grant is read back through the run's own grant list — which
// re-proves it belongs to this run. It selects a namespace and reads no value.
//
// Without that answer the read is refused, never guessed: reading a grant that
// is shared as if it were not would look in the run owner's own namespace
// first, which is exactly what `shared` exists to rule out. A list that cannot
// be read refuses as an unreachable store does; a list that does not hold the
// grant refuses outright. A deployment with no store keeps no grant rows and
// has no shared grants.
func (s *Server) injectionReadFor(ctx context.Context, claims *identity.Claims, grantID uuid.UUID, minted broker.Minted) (injectionRead, error) {
	var scope json.RawMessage
	if s.cfg.Store != nil {
		grants, err := s.cfg.Store.ListGrantsByRun(ctx, claims.RunID)
		if err != nil {
			return injectionRead{ownRowOnly: true, unknown: true}, fmt.Errorf("%w: read the run's grants: %w", secretstore.ErrUnavailable, err)
		}
		i := slices.IndexFunc(grants, func(g types.CredentialGrant) bool { return g.ID == grantID })
		if i < 0 {
			return injectionRead{ownRowOnly: true, unknown: true}, errGrantNotOfRun
		}
		scope = grants[i].Spec.Scope
	}
	owner, ownRowOnly := injectionGrantRead(scope, claims.Sub, minted.OwnerOnly, claims.OperatorOwned)
	return injectionRead{owner: owner, ownRowOnly: ownRowOnly, shared: apiKeyScopeShared(scope)}, nil
}

// storedKeyTTL is how long the proxy may inject a stored key before it asks
// the sink again (CS-4). A stored key carries no expiry of its own, so before
// this the proxy held it for the run's whole life, and a key removed, replaced
// or refused at the store kept working in every run already using it.
const storedKeyTTL = 10 * time.Minute

// storedKeyExpiry is the ExpiresAt the sink gives a stored key: storedKeyTTL
// from now. The proxy re-resolves ahead of it, and a definitive refusal on that
// re-resolve stops the injection at once.
//
// An approval-gated grant stays static (0): each of its mints spends the
// approval (broker.ErrAlreadyMinted unless a run-wide lease covers it), so a
// re-resolve would fail the run at the first expiry. Its key is read once per
// approval, as before.
func (s *Server) storedKeyExpiry(m broker.Minted) int64 {
	if m.ApprovalID != uuid.Nil {
		return 0
	}
	return s.cfg.Now().Add(storedKeyTTL).UnixMilli()
}

// subscriptionLease is the ExpiresAt the sink gives a subscription token: the
// stored-key lease, or the token's own expiry when that is sooner (or the
// grant is approval-gated and so has no lease). A stored sign-in, the managed
// setup-token and a person's own alike, is re-read on the stored-key clock.
func (s *Server) subscriptionLease(minted broker.Minted, tok subscription.Token) int64 {
	lease := s.storedKeyExpiry(minted)
	if tok.ExpiresAt.IsZero() {
		return lease
	}
	if exp := tok.ExpiresAt.UnixMilli(); lease == 0 || exp < lease {
		return exp
	}
	return lease
}

// sinkStoreUnreachable is SINK.KEK_UNREACHABLE (credential-storage design §3).
const sinkStoreUnreachable = "Wardyn couldn't reach the service that holds this run's credential, so it couldn't unlock it. Nothing was substituted. Try again in a moment."

// storeReadRefusal is the sink's answer to a failed read of a stored
// credential, split the way the proxy acts on it (design K8, rules 10 and 21).
// A 503 means the store did not answer: transient, and the proxy keeps serving
// its last-good header for a bounded grace. Every other status is definitive —
// the credential is gone, or the store refused it (binding mismatch, access
// denied, disabled) — and the proxy drops the header at once, so revoking
// Wardyn's access at the store bites without waiting out the grace.
func storeReadRefusal(name string, err error) (status int, reason, body string) {
	switch {
	case errors.Is(err, secretstore.ErrUnavailable):
		return http.StatusServiceUnavailable, reasonSinkStoreUnavailable, sinkStoreUnreachable
	case errors.Is(err, secretstore.ErrNotFound):
		return http.StatusFailedDependency, reasonSinkSecretNotFound, "secret " + name + " is not in the store (set it with `wardyn secret set`)"
	default:
		// The row exists: re-setting it would overwrite what an operator may need to inspect.
		return http.StatusFailedDependency, reasonSinkSecretRefused, "secret " + name + " exists but could not be used: the store refused it " +
			"(its value is gone or bound to another row, or Wardyn's access to it was revoked). Nothing was substituted; ask an admin to check it."
	}
}
