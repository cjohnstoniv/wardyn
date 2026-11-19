// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

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
// the authority an extension of the run's end needs (extendRefusal): the captured profile, its
// placement walls, the launch-door capabilities and the model provider. The capabilities are asked only
// of the owner's own session (resumeAuthorityRefusal). An owner who is deactivated or suspended, or a
// service identity that is suspended or retired, ends the run through the kill cascade. Authority the
// owner no longer holds leaves the run paused and refused, with one denied row per pause
// (pauseClocks.firstDenial). A check that cannot be answered refuses with 503 and writes no row; the
// next resume asks again.
func (s *Server) resumeRefusal(ctx context.Context, run types.AgentRun, actorType types.ActorType, principal string, data map[string]any) error {
	live, err := s.resumeOwnerLive(ctx, run)
	if err != nil {
		return &errResumeRefused{&ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
			msg: loggedMsg(ctx, "re-check the run owner's identity", err)}}
	}
	var ref *ownerRefusal
	switch {
	case live:
		ref = s.resumeAuthorityRefusal(ctx, run)
	case isServicePrincipal(run.CreatedBy):
		ref = &ownerRefusal{status: http.StatusForbidden, reason: reasonServiceIdentityInactive, msg: resumeServiceInactiveMsg}
	default:
		ref = &ownerRefusal{status: http.StatusForbidden, reason: reasonIdentityDeactivated, msg: identityDeactivatedMsg}
	}
	if ref == nil {
		return nil
	}
	if ref.status != http.StatusServiceUnavailable && s.pause.firstDenial(run.ID, run.PausedAt) {
		data["subject"], data["refusal"] = run.CreatedBy, ref.reason
		s.recordAuditHeld(ctx, s.auditEvent(&run.ID, actorType, principal, "run.resume", run.ID.String(), "denied", mustJSON(data)))
	}
	if !live {
		// The run's lock is held (resumeRun's callers); the kill takes no lock before it (db.LockOrder).
		s.pause.dropDenial(run.ID)
		if _, _, kerr := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": ref.reason}); kerr != nil {
			return &errResumeRefused{&ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
				msg: loggedMsg(ctx, "end the run of an owner who lost access", kerr)}}
		}
	}
	return &errResumeRefused{ref}
}

// resumeAuthorityRefusal is extendRefusal for a resume. Only the owner's own session resolves their
// capabilities as launch did (their groups, their email and an admin's exemption; ownerCapabilityRefusal),
// and a resume is mostly not theirs: an approver's decision, the sweep, a gateway's wake-up or a super
// admin's Resume. Without it the capabilities are not asked, since rows the owner holds through a group
// would refuse a run they may still run; the profile and the model provider, which read no capability
// rows, are asked of every resume. An operator-owned run is never the owner's session (created_by can
// be spelled alike by an IdP sub).
func (s *Server) resumeAuthorityRefusal(ctx context.Context, run types.AgentRun) *ownerRefusal {
	if !run.OperatorOwned && run.CreatedBy != "" && oidcHumanFromContext(ctx) == run.CreatedBy {
		return s.extendRefusal(ctx, run, true)
	}
	if ref := s.ownerCeilingRefusal(ctx, run); ref != nil {
		return ref
	}
	ref, err := s.modelProviderRefusal(ctx, run)
	if err != nil {
		return &ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
			msg: loggedMsg(ctx, "re-check the run's model provider", err)}
	}
	return ref
}

// firstDenial reports whether a refused resume of run id owes its run.resume denied row: the first
// refusal of the pause that began at pausedAt, never one per retry. ponytail: per process, like the
// other pause clocks; a replica that takes the sweep over writes the pause's row once more.
func (c *pauseClocks) firstDenial(id uuid.UUID, pausedAt *time.Time) bool {
	var at time.Time
	if pausedAt != nil {
		at = *pausedAt
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.denied[id]; ok && prev.Equal(at) {
		return false
	}
	if c.denied == nil {
		c.denied = map[uuid.UUID]time.Time{}
	}
	c.denied[id] = at
	return true
}

// dropDenial forgets run id's refused pause once the run resumes or ends.
func (c *pauseClocks) dropDenial(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.denied, id)
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
