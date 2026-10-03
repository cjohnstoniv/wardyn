// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeSessionsRequest is POST /api/v1/sessions/revoke's body: exactly one
// of Sub (revoke a single principal's sessions) or All (revoke every
// principal's sessions) must be set — see handleRevokeSessions.
type revokeSessionsRequest struct {
	Sub string `json:"sub"`
	All bool   `json:"all"`
}

// handleRevokeSessions is the admin surface for "revoke a human now" — the
// OIDC session cookie is stateless (see internal/auth/oidc's package doc), so
// there is no session row to delete; instead this stamps a CUTOFF
// (Config.SessionRevocations) that oidc.Authenticator.Middleware checks on
// every authenticated request, so a still-unexpired session for the target
// stops working on its VERY NEXT request rather than lingering until its own
// Expiry (up to the OIDC-configured token lifetime) — the same behavior a
// logout, an IdP role demotion, or an IdP account disablement could not
// otherwise force before this existed.
//
// It also deletes the target's registered SSH keys and revokes every
// unrevoked per-user API token the target holds: a
// wdn_ bearer authenticates AS that human (apiTokenAuth). Both credential
// lanes also check the cutoff, so a registration that races the sweep cannot
// leave usable access behind.
//
// "sub" names either identity — the OIDC sub or the email — and both halves
// below honour that: the cutoff is matched against both by IsSessionRevoked,
// and the token sweep falls back to api_tokens.email. It is the rule every
// other user-addressing surface already follows (a subject_type=user capability
// grant matches the sub OR the email, precisely so an admin need not guess
// which the IdP made authoritative). Keyed on sub alone, this silently fails on
// any IdP where the two differ — Entra, whose sub is an opaque per-app
// identifier, is the deployment shape the SSO work targets — reporting success
// while the compromised human's console session and every wdn_ token stay live.
//
// What CANNOT be answered here is "did that name anybody" — sessions are
// stateless signed cookies with no row to count, so a target that matches
// nobody is indistinguishable from one whose sessions have all expired. The
// audit row carries tokens_revoked and ssh_keys_deleted for the countable
// credentials; unresolved or ambiguous SSH principal lookups fail rather than
// pretending to have removed that access. A zero
// there against a human you believe holds tokens is the signal that the
// identifier was wrong.
//
// Mounted only when OIDC + SessionRevocations are both wired (see routes.go)
// — with no OIDC session mechanism there is nothing to revoke.
//
// No target-role guard, deliberately — and this is the SECURITY_ADMIN tier
// (securityOps, routes.go), so a security admin may cut a SUPER ADMIN's
// sessions and tokens, and the All arm below cuts every principal's. Asked
// directly: should a security admin be refused a target in the tier above?
// No, and the reasons are structural rather than a judgement call:
//
//   - Revocation only SUBTRACTS. It hands the caller nothing — no session, no
//     token, no reach — which is the exact property that put this route on
//     securityOps at all. A guard here would protect no capability; it would
//     only decide who may perform an audited subtraction.
//   - Incident response IS this tier's job. RevokeAll's own doc calls it "the
//     incident-response 'log everyone out' lever", and a lever that exempts the
//     most privileged accounts is not one — a compromised super-admin session
//     is precisely the case you buy it for.
//   - The tiers deliberately do not nest (routes.go's securityOps rationale).
//     "security_admin may not act on admin" would be a ladder assertion, and
//     the ladder is the shape this design refuses; the asymmetry it DOES keep
//     is the one that matters — a security admin's SSH key and attach ticket
//     still stamp member, so this tier never yields a shell in a foreign
//     sandbox.
//   - It is not a lockout, so it cannot be used to hold the deployment. The
//     cutoff is a TIMESTAMP (oidc.SessionRevocations.IsSessionRevoked), so the
//     target signs in again and their new session's issued-at clears it; and
//     adminAuth (http.go) never consults revocations, so the admin bearer
//     break-glass survives even the All arm. A rogue security admin cannot
//     revoke their way to an un-revertible position.
//
// What a guard here WOULD have cost is the case it exists for: a super admin
// whose session is the compromised one, at 3am, with the security admin the
// only person on call.
//
// API tokens and SSH keys are the reason the All arm is an incident
// lever rather than a routine one: unlike sessions they do not self-heal, and
// every automation credential must be re-minted and SSH key re-registered.
// Pinned by TestSecurityAdminRevokesSuperAdmin; stated for operators in
// docs/OPERATIONS.md's security-admin section.
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	var body revokeSessionsRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	body.Sub = strings.TrimSpace(body.Sub)
	if body.All == (body.Sub != "") {
		writeErrorReason(w, http.StatusBadRequest, reasonSessionsRevokeParamInvalid, `body must set exactly one of "sub" or "all"`)
		return
	}
	scope, target := "sub", body.Sub
	actorCtx := withRequestActor(r)
	var res personRevocation
	var err error
	if body.All {
		scope, target = "all", "*"
		if err = s.cfg.SessionRevocations.RevokeAll(r.Context()); err != nil {
			writeServerError(w, r, "revoke sessions", err)
			return
		}
		res, err = s.revokeCredentials(actorCtx, "")
	} else {
		res, err = s.revokePersonCredentials(actorCtx, body.Sub)
		if !res.Stamped {
			writeServerError(w, r, "revoke sessions", err)
			return
		}
		// The route has always refused on an unresolved key owner, once the
		// cutoff has committed; only the request-free callers read it as zero keys.
		if res.KeyUnresolved {
			err = errors.Join(err, errors.New(res.Refusal))
		}
	}
	refusal, keyReason := res.Refusal, res.KeyReason
	tokens, keys := res.Tokens, res.Keys
	data := map[string]any{"scope": scope, "tokens_revoked": tokens, "ssh_keys_deleted": keys}
	if !body.All {
		data["sub"] = body.Sub
	}
	outcome := "success"
	if err != nil {
		outcome, data["error"] = "failure", err.Error()
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"session.revoke", target, outcome, mustJSON(data)))
	if err != nil {
		// A resolver refusal is a composed product sentence, not driver text —
		// the caller gets it verbatim rather than the generic "revoke credentials"
		// (the cutoff already committed, so the responder needs the remedy, not
		// just the log line).
		if refusal != "" {
			writeErrorReason(w, http.StatusInternalServerError, keyReason, refusal)
			return
		}
		writeServerError(w, r, "revoke credentials", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// personRevocation is what one revokePersonCredentials (or the revoke-all
// sweep) did. Stamped says the session cutoff committed; Refusal and KeyReason
// are the SSH key owner resolver's answer, and KeyUnresolved marks the refusal
// that names a person with nothing on this deployment.
type personRevocation struct {
	Tokens, Keys  int
	Stamped       bool
	Refusal       string
	KeyReason     string
	KeyUnresolved bool
}

// revokePersonCredentials is the credential sequence POST /sessions/revoke runs
// for one person, callable without a request: the session cutoff, then the
// person's API tokens, then their SSH keys, then the cutoff again under the
// principal the keys resolved to. Audit rows name the actor withActor attached
// to ctx.
//
// An empty principal is refused and changes nothing: the helpers below read ""
// as everyone, and that lever stays in handleRevokeSessions. An unresolved key
// owner is zero keys and success; an ambiguous one is a failure.
func (s *Server) revokePersonCredentials(ctx context.Context, principal string) (personRevocation, error) {
	if principal == "" {
		return personRevocation{}, errors.New("revoke person credentials: principal required")
	}
	// Without an attached actor the audit rows would fall back to the admin
	// token, misattributing a request-free caller in the append-only audit.
	if _, ok := ctx.Value(auditActorCtxKey{}).(auditActor); !ok {
		return personRevocation{}, errors.New("revoke person credentials: audit actor required")
	}
	if err := s.cfg.SessionRevocations.RevokeSub(ctx, principal); err != nil {
		return personRevocation{}, err
	}
	res, err := s.revokeCredentials(ctx, principal)
	res.Stamped = true
	return res, err
}

// revokeCredentials sweeps tokens and keys for principal ("" is everyone). Each
// lane still runs if the other fails. The cutoff has already committed, so the
// audit records the completed work before returning failure.
func (s *Server) revokeCredentials(ctx context.Context, principal string) (personRevocation, error) {
	var res personRevocation
	var tokenErr error
	res.Tokens, tokenErr = s.revokeAPITokensFor(ctx, principal)
	var keyPrincipal string
	var keyErr error
	res.Keys, keyPrincipal, res.Refusal, res.KeyReason, keyErr = s.deleteSSHKeysFor(ctx, principal)
	// SSH keys have no email column. Preserve the named cutoff for sessions and
	// tokens, and stamp the resolved subject to catch registrations the DELETE missed.
	if principal != "" && keyPrincipal != "" && keyPrincipal != principal {
		keyErr = errors.Join(keyErr, s.cfg.SessionRevocations.RevokeSub(ctx, keyPrincipal))
	}
	if res.Refusal != "" {
		res.KeyUnresolved = res.KeyReason == reasonOwnerUnresolved
		if !res.KeyUnresolved {
			keyErr = errors.New(res.Refusal)
		}
	}
	return res, errors.Join(tokenErr, keyErr)
}

// revokeAPITokensFor revokes every unrevoked API token owned by principal, or
// by everyone when principal is "" (the revoke-all arm). Returns how many it
// revoked. ponytail: one list + a loop over the existing admin-scoped
// RevokeAPIToken rather than a new bulk store method — a principal holds a
// handful of tokens, never thousands.
//
// principal is whichever identity the caller named, and api_tokens carries
// BOTH (principal = the IdP sub, email = the address that minted it, migration
// 0045), so an email-form target has to be matched on the email column or the
// sweep silently finds nothing.
//
// One unconditional union, not a fallback: the email arm must run regardless of
// whether the principal lookup found anything, or a human whose tokens straddle
// both principal forms — one row minted under the email form, one under the
// IdP's opaque sub, the ordinary Entra shape — would have the other form's rows
// silently left live while the response and audit row both read success. The
// session cutoff has always matched sub OR email (IsSessionRevoked); this is the
// token half agreeing with it.
//
// ListAPITokensByPrincipal is UNFILTERED (handleCreateAPIToken's own
// `t.RevokedAt == nil` loop is the proof), so a target with nothing but revoked
// rows would otherwise suppress the email arm too.
//
// Cost: one extra ListAPITokens call in the sub-form case, buying correctness
// on the identity-straddling shape this lever exists to cover.
//
// The sweep is not the only closure. A token may carry an expiry, but most carry none, and
// apiTokenAuth now compares each row's created_at against the SAME cutoff this
// handler stamps, so a mint whose INSERT commits after this snapshot is taken —
// unreachable by this sweep forever, since nothing ever re-listed — stops
// authenticating anyway. The sweep is what makes GET /api/v1/tokens SHOW the
// row revoked; the read-side check is what makes the lever true.
func (s *Server) revokeAPITokensFor(ctx context.Context, principal string) (int, error) {
	var (
		toks []types.APIToken
		err  error
	)
	if principal == "" {
		toks, err = s.cfg.Store.ListAPITokens(ctx)
	} else {
		toks, err = s.cfg.Store.ListAPITokensByPrincipal(ctx, principal)
	}
	if err != nil {
		return 0, err
	}
	if principal != "" {
		// The caller may have named either identity, so BOTH are matched, every
		// time. Case-insensitively on email, exactly as IsSessionRevoked's email
		// arm does, so the two halves of one revoke cannot disagree about who
		// was named.
		all, aerr := s.cfg.Store.ListAPITokens(ctx)
		if aerr != nil {
			return 0, aerr
		}
		seen := make(map[uuid.UUID]bool, len(toks))
		for _, t := range toks {
			seen[t.ID] = true
		}
		for _, t := range all {
			if t.Email != "" && strings.EqualFold(t.Email, principal) && !seen[t.ID] {
				seen[t.ID] = true
				toks = append(toks, t)
			}
		}
	}
	now := time.Now().UTC()
	n := 0
	for _, t := range toks {
		if t.RevokedAt != nil {
			continue
		}
		revoked, err := s.cfg.Store.RevokeAPIToken(ctx, t.ID, "", now)
		if err != nil {
			return n, err
		}
		// One row per credential, naming its owner: an aggregate `tokens_revoked`
		// on the caller's own event alone cannot answer "which credentials did
		// that edit kill" for the role-mapping lane's unanswerable-snapshot arm,
		// which revokes EVERY elevated-stamp or old-type-stamped token in the
		// deployment, whoever holds it — a table is a state, not a record of
		// who did it or when.
		// Same action and same two keys as the single-token door
		// (handleRevokeAPIToken), plus the scope that says this was a sweep, so
		// one query answers the question across both doors.
		actorType, actor := auditActorFromContext(ctx)
		s.recordAudit(ctx, s.auditEvent(nil, actorType, actor,
			"token.revoke", revoked.ID.String(), "success", mustJSON(map[string]any{
				"principal": revoked.Principal, "name": revoked.Name, "scope": "sweep"})))
		n++
	}
	return n, nil
}
