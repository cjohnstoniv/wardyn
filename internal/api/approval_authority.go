// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deciderAuthorityCapped reports whether a decider basis is one a unit's deciders rule gives a person by their
// place in the unit (unit_admins) or a group (group:<name>). Those deciders approve an egress host only within
// their own egress_host grant (#4073). The owner meets the grant in resolveDecider already; the security tier,
// the admin token and an approval rule are not capped (the security tier writes the grants).
func deciderAuthorityCapped(basis string) bool {
	if strings.HasPrefix(basis, deciderBasisUnitAdminOf) {
		return true
	}
	_, ok := types.DeciderGroupOf(basis)
	return ok
}

// deciderForDecideWithAuthority is deciderForDecide followed by admitDeciderAuthority, so decide takes both
// with one refusal branch. It answers the verdict, the scope, and the host an audit alert names with whether
// one is owed.
func (s *Server) deciderForDecideWithAuthority(w http.ResponseWriter, r *http.Request, id uuid.UUID, ap types.ApprovalRequest, run *types.AgentRun, haveRun *bool, scope types.ApprovalScope, approve bool) (deciderVerdict, types.ApprovalScope, string, bool, bool) {
	dv, scope, ok := s.deciderForDecide(w, r, id, ap, run, haveRun, scope)
	if !ok {
		return dv, scope, "", false, false
	}
	alertHost, alert, ok := s.admitDeciderAuthority(w, r, id, ap, dv, approve)
	return dv, scope, alertHost, alert, ok
}

// admitDeciderAuthority caps a unit or group decider's approve of an egress_domain hold by their own egress_host
// grant on the approval's host, the check the run's owner meets in resolveDecider and a member meets with no
// rule in authorizeUserDecision. The host is the approval's own RequestedScope, never the body; an absent or
// malformed one is "", which only a `*` grant covers. A deny narrows and is never capped. With egress_host
// unenforced and no rows the grant answers true, so a deployment that never wrote one sees no change.
//
// ok is false only after it has written the refusal. Under WARDYN_IDENTITY_NARROWING=audit a gap answers
// alert true with the host, "" included: the decision goes ahead and the caller writes approval.authority.alert
// once it is made. Under enforce the gap is refused before the one-way line, so the approval stays PENDING. A
// capability read error refuses under enforce; under audit it is logged and the decision goes ahead with no
// alert, since a rule-named decider's approve made no capability read before this check.
func (s *Server) admitDeciderAuthority(w http.ResponseWriter, r *http.Request, id uuid.UUID, ap types.ApprovalRequest, dv deciderVerdict, approve bool) (alertHost string, alert, ok bool) {
	if !approve || ap.Kind != types.ApprovalEgressDomain || !deciderAuthorityCapped(dv.basis) {
		return "", false, true
	}
	enforce := identityNarrowingEnforced()
	host := approvalHost(ap)
	allowed, err := s.capSeamAllowed(r.Context(), capEgressHost, host)
	if err != nil {
		if enforce {
			writeServerError(w, r, "resolve capability", err)
			return "", false, false
		}
		slog.WarnContext(r.Context(), "api: decider authority not checked; the decision goes ahead",
			"approval_id", id, "basis", dv.basis, "err", err)
		return "", false, true
	}
	if allowed {
		return "", false, true
	}
	if !enforce {
		return host, true, true
	}
	s.refuse(w, r, authz.Deny(authz.ReasonCapabilityEgressHost, id.String(), "you are not granted egress host "+host+
		" — an admin decides this one, or can grant it to you").OnRun(ap.RunID).With("host", host).With("basis", dv.basis))
	return "", false, false
}

// recordDeciderAuthorityAlert writes approval.authority.alert for a decision admitDeciderAuthority let through
// under audit: the decider approved a host their own egress_host grants do not cover, and the approval stands.
// It writes nothing when no alert is owed.
func (s *Server) recordDeciderAuthorityAlert(r *http.Request, runID uuid.UUID, id uuid.UUID, dv deciderVerdict, host string, alert bool) {
	if !alert {
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(&runID, actorType, actor, "approval.authority.alert", id.String(), "success",
		mustJSON(map[string]any{
			"approval_id": id.String(), "basis": dv.basis, "host": host, "mode": identityNarrowingMode(),
		})))
}
