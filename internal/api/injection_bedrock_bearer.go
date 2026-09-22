// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Bedrock BEARER resolve — the sink half of the grant
// authorBedrockBearerInjection authors at dispatch.
//
// bedrock-api-key is the one stored name whose NAMESPACE a roster row decides:
// the operator's under `shared`, the run owner's own under `per_user`. The
// generic sink's read, Store.For(sub).Get, cannot keep that promise, because
// the owner's row wins when there is one and the operator's fills in when there
// is not. Resolved through it, the sink could pick a different key from the one
// dispatch chose: a member's own key in place of the operator's under shared,
// the operator's in place of a member's deleted one, and the operator's again
// for any grant naming the key on a run whose own agent row is not per_user.
//
// So dispatch RECORDS the namespace the bearer was read from on its own grant,
// and this is the only place the name resolves: from exactly that namespace,
// through bedrockBearerFor — the read dispatch made — and never through the
// fallback. A grant with no record was not authored by dispatch (a stored or
// inline policy, a recorded profile, a hand-written row); dispatch drops those
// from the proxy's injections, and this refuses one if it arrives anyway.

// bedrockBearerSnapshot is the namespace dispatch read a run's Bedrock bearer
// from, recorded under the grant scope's "snapshot" key. Like
// awsSSOScopeSnapshot it records a decision and grants nothing: the resolve
// still requires it to equal what the live roster names for the run's own
// subject, so a hand-written one can name no namespace but the one dispatch
// would have chosen.
type bedrockBearerSnapshot struct {
	OwnerSubject     string `json:"owner_subject"`
	CredentialSource string `json:"credential_source"`
}

func bedrockBearerSnapshotOf(sc awsSSOScope) bedrockBearerSnapshot {
	return bedrockBearerSnapshot{OwnerSubject: sc.owner, CredentialSource: awsSSOCredentialSourceLabel(sc)}
}

// DRAFT (M2 canon pending)
const (
	// bedrockBearerNamespaceNotOwn is the refusal when a per-user run's own key
	// is gone. It names the namespace rather than the secret's existence: the
	// generic not-in-the-store sentence would tell the reader to set the secret,
	// which is right, but not that the operator's key is deliberately not
	// standing in.
	bedrockBearerNamespaceNotOwn = "this agent's model credential is one per person and your own Bedrock API key is not in the store " +
		"(store it under Settings → Model provider → AWS Bedrock → Bedrock bearer key); the operator's key does not stand in for it"
	// bedrockBearerNotRecorded refuses a grant dispatch did not author.
	bedrockBearerNotRecorded = "bedrock-api-key is injected only through the grant Wardyn authors when a run launches on the " +
		"Bedrock bearer key, which records whose key the run uses; this grant carries no such record"
	bedrockBearerOperatorAbsent = "secret " + bedrockAPIKeySecret + " is not in the store (set it with `wardyn secret set`)"
)

// resolveBedrockBearerInjection is the bedrock-api-key arm of
// handleInternalInjection. handled=false means the grant names another secret
// and the generic path must run; handled=true means a response was written.
//
// Fail CLOSED on an unreadable run or roster: the zero SiteConfig is
// indistinguishable from "a roster with no row", whose scope is the OPERATOR
// namespace — the substitution this refuses.
func (s *Server) resolveBedrockBearerInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	if minted.Injection.SecretName != bedrockAPIKeySecret {
		return false
	}
	ctx := r.Context()
	fail := func(status int, reason, body string) bool {
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", bedrockAPIKeySecret, "failure",
			mustJSON(map[string]any{"reason": reason, "grant_id": grantID})))
		writeError(w, status, body)
		return true
	}

	var recorded bedrockBearerSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &recorded) || recorded.CredentialSource == "" {
		return fail(http.StatusForbidden, "missing_scope_snapshot", bedrockBearerNotRecorded)
	}
	run, rerr := s.cfg.Store.GetRun(ctx, claims.RunID)
	if rerr != nil {
		return fail(http.StatusServiceUnavailable, "run_unreadable", credentialReauthRunUnreadableBody)
	}
	siteCfg, scErr := s.cfg.Store.GetSiteConfig(ctx)
	if scErr != nil {
		return fail(http.StatusServiceUnavailable, "roster_unreadable", credentialReauthStoreErrorBody)
	}
	// The recorded namespace must still be the one the roster names for this
	// run's OWN subject (the run token, not the grant, is authority for who that
	// is), and a per_user row must still declare the bearer lane. A roster that
	// moved mid-run refuses rather than re-pointing a run in flight — the rule
	// resolveAWSSSOInjection enforces for the session lane.
	scope := awsSSOScopeFor(siteCfg, run.Agent, claims.Sub)
	if bedrockBearerSnapshotOf(scope) != recorded || !scope.readsBearer() {
		return fail(http.StatusForbidden, "scope_changed", credentialReauthScopeChangedRefusal)
	}
	secret := s.bedrockBearerFor(ctx, scope)
	if len(secret) == 0 {
		if scope.perUser {
			return fail(http.StatusFailedDependency, "per_user_bearer_absent", bedrockBearerNamespaceNotOwn)
		}
		return fail(http.StatusFailedDependency, "bearer_absent", bedrockBearerOperatorAbsent)
	}

	formatted := formatInjectionValue(minted.Injection.Format, secret)
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, secret)
		if formatted != string(secret) {
			s.cfg.MaskRegistry.Add(claims.RunID, []byte(formatted))
		}
	}
	// owner is WHOSE key was read ("" = the operator's), not the run's owner:
	// under shared those differ, and the trail must name the one billed.
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", bedrockAPIKeySecret, "success",
		mustJSON(map[string]any{
			"purpose": "proxy-injection", "grant_id": grantID, "jti": minted.JTI,
			"owner": recorded.OwnerSubject, "credential_source": recorded.CredentialSource,
		})))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host:   minted.Injection.Host,
		Header: minted.Injection.Header,
		Value:  formatted,
		JTI:    minted.JTI,
	})
	return true
}
