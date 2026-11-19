// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errResumeRefused is resumeRun's answer when the run's owner no longer holds the authority it was
// launched with: ref says why, in the words and status the Resume button answers with.
type errResumeRefused struct{ ref *ownerRefusal }

func (e *errResumeRefused) Error() string { return e.ref.msg }

// resumeServiceInactiveMsg answers a resume of a run whose service identity lost its authority.
const resumeServiceInactiveMsg = "The service identity that started this run is suspended or retired, so the run was ended"

// resumeRefusal re-checks, before a paused run is thawed, what a pause may have outlived: the owner's
// identity, as a pool start re-checks it (queueOwnerLive; serviceRunLive for a service identity), and
// the captured profile, as an extension of the run's end re-checks it (ownerCeilingRefusal). An owner
// who is deactivated or suspended, or a service identity that is suspended or retired, ends the run
// through the kill cascade. A profile that no longer admits the run leaves it paused and refused. A
// check that cannot be answered refuses with 503 and writes no row; the next resume asks again.
func (s *Server) resumeRefusal(ctx context.Context, run types.AgentRun, actorType types.ActorType, principal string, data map[string]any) error {
	live, err := s.resumeOwnerLive(ctx, run)
	if err != nil {
		return &errResumeRefused{&ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
			msg: loggedMsg(ctx, "re-check the run owner's identity", err)}}
	}
	var ref *ownerRefusal
	switch {
	case live:
		ref = s.ownerCeilingRefusal(ctx, run)
	case isServicePrincipal(run.CreatedBy):
		ref = &ownerRefusal{status: http.StatusForbidden, reason: reasonServiceIdentityInactive, msg: resumeServiceInactiveMsg}
	default:
		ref = &ownerRefusal{status: http.StatusForbidden, reason: reasonIdentityDeactivated, msg: identityDeactivatedMsg}
	}
	if ref == nil {
		return nil
	}
	if ref.status != http.StatusServiceUnavailable {
		data["subject"], data["refusal"] = run.CreatedBy, ref.reason
		s.recordAuditHeld(ctx, s.auditEvent(&run.ID, actorType, principal, "run.resume", run.ID.String(), "denied", mustJSON(data)))
	}
	if !live {
		// The run's lock is held (resumeRun's callers); the kill takes no lock before it (db.LockOrder).
		if _, _, kerr := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": ref.reason}); kerr != nil {
			return &errResumeRefused{&ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
				msg: loggedMsg(ctx, "end the run of an owner who lost access", kerr)}}
		}
	}
	return &errResumeRefused{ref}
}

// resumeOwnerLive reports whether the run's owner may still act: a person not deactivated and with no
// session cut since the run was created, a service identity neither suspended nor retired and with a
// live sponsor. An operator-owned run (the admin token, local mode) has no one to revoke.
func (s *Server) resumeOwnerLive(ctx context.Context, run types.AgentRun) (bool, error) {
	if run.OperatorOwned {
		return true, nil
	}
	if isServicePrincipal(run.CreatedBy) {
		live, err := s.serviceRunLive(ctx, run)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return live, err
	}
	return s.queueOwnerLive(ctx, queueAuthority{Sub: run.CreatedBy}, run.CreatedAt)
}
