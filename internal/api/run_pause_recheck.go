// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
// identity (IdentityBlocked for a person; for a service identity its record, its sponsorship and its
// sponsor's IdentityBlocked; no session revoke is asked, resumeOwnerLive), and the authority an
// extension of the run's end needs (extendRefusal): the captured profile, its placement walls, the
// launch-door capabilities and the model provider. The capabilities are asked only of the owner's own
// session (resumeAuthorityRefusal). An owner who is deactivated or suspended, or a service identity
// that is suspended or retired, ends the run through the kill cascade. Authority the owner no longer
// holds leaves the run paused and refused, with one denied row per pause (pauseClocks.firstDenial). A
// check that cannot be answered, within one heldStoreCallBound for all its reads (the run's lock is
// held), refuses with 503 and writes no row; the next resume asks again.
func (s *Server) resumeRefusal(ctx context.Context, run types.AgentRun, actorType types.ActorType, principal string, data map[string]any) error {
	live, ref := s.resumeRecheck(ctx, run)
	dead := !live && ref == nil // the owner was read and is not live; an unreadable check is a 503 ref
	switch {
	case !dead:
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
	if dead {
		// The run's lock is held (resumeRun's callers); the kill takes no lock before it (db.LockOrder).
		s.pause.dropDenial(run.ID)
		if _, _, kerr := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": ref.reason}); kerr != nil {
			return &errResumeRefused{&ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
				msg: loggedMsg(ctx, "end the run of an owner who lost access", kerr)}}
		}
	}
	return &errResumeRefused{ref}
}

// resumeRecheck is resumeRefusal's reads, on one heldStoreCallBound: whether the owner is live, and for a
// live owner what refuses the resume (nil when nothing does). A read that fails or runs out of the bound
// is a 503 refusal.
func (s *Server) resumeRecheck(ctx context.Context, run types.AgentRun) (bool, *ownerRefusal) {
	ctx, cancel := context.WithTimeout(ctx, heldStoreCallBound)
	defer cancel()
	live, err := s.resumeOwnerLive(ctx, run)
	if err != nil {
		return false, &ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
			msg: loggedMsg(ctx, "re-check the run owner's identity", err)}
	}
	if !live {
		return false, nil
	}
	return true, s.resumeAuthorityRefusal(ctx, run)
}

// resumeAuthorityRefusal is extendRefusal for a resume. Only the owner's own session resolves their
// capabilities as launch did (their groups, their email and an admin's exemption; ownerCapabilityRefusal),
// and a resume is mostly not theirs: an approver's decision, the sweep, a gateway's wake-up or a super
// admin's Resume. Without it the capabilities are not asked, since rows the owner holds through a group
// would refuse a run they may still run; the profile, the run's organisation components and the model
// provider, which read no capability rows, are asked of every resume. An operator-owned run is never the owner's session (created_by can
// be spelled alike by an IdP sub).
func (s *Server) resumeAuthorityRefusal(ctx context.Context, run types.AgentRun) *ownerRefusal {
	if !run.OperatorOwned && run.CreatedBy != "" && oidcHumanFromContext(ctx) == run.CreatedBy {
		return s.extendRefusal(ctx, run, true)
	}
	if ref := s.ownerCeilingRefusal(ctx, run); ref != nil {
		return ref
	}
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	var ref *ownerRefusal
	if err == nil {
		ref, err = s.componentGoneRefusal(ctx, comps)
	}
	if err == nil && ref == nil {
		ref, err = s.componentPinRefusal(ctx, run, comps)
	}
	if err == nil && ref == nil {
		ref, err = s.modelProviderRefusal(ctx, run)
	}
	if err != nil {
		return &ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
			msg: loggedMsg(ctx, "re-check the run's components and model provider", err)}
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
	if c.denied == nil || len(c.denied) >= 4096 {
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

// resumeOwnerLive reports whether the run's owner may still act: a person whose identity is not
// deactivated or suspended (IdentityBlocked), or a service identity that resumeServiceLive finds live.
// A revoke of sessions, the owner's or a sponsor's, is not asked: it signs them out, and their running
// runs go on (docs/operations/delegation.md). An operator-owned run (the admin token, local mode) has no
// one to revoke.
func (s *Server) resumeOwnerLive(ctx context.Context, run types.AgentRun) (bool, error) {
	if run.OperatorOwned {
		return true, nil
	}
	if name, ok := strings.CutPrefix(run.CreatedBy, types.ServicePrincipalPrefix); ok {
		return s.resumeServiceLive(ctx, run, name)
	}
	return s.resumeNotBlocked(ctx, run.CreatedBy)
}

// resumeServiceLive is serviceRunLive's identity and sponsor reads without its session checks: the
// service identity is neither suspended nor retired, is still sponsored by the person who sponsored it
// when the run started (ServiceStillSponsored), and neither it nor that sponsor is blocked.
func (s *Server) resumeServiceLive(ctx context.Context, run types.AgentRun, name string) (bool, error) {
	si, err := s.serviceRecord(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil || !si.Usable():
		return false, err
	}
	// serviceRecord answered, so the store is a ServiceIdentityStore.
	switch still, err := s.cfg.Store.(store.ServiceIdentityStore).ServiceStillSponsored(ctx, si.Name, si.Sponsor, run.CreatedAt); {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil || !still:
		return false, err
	}
	if live, err := s.resumeNotBlocked(ctx, run.CreatedBy); err != nil || !live {
		return false, err
	}
	return s.resumeNotBlocked(ctx, si.Sponsor)
}

// resumeNotBlocked reports whether principal's identity is neither deactivated nor suspended.
func (s *Server) resumeNotBlocked(ctx context.Context, principal string) (bool, error) {
	ib, ok := s.cfg.Store.(identityBlocker)
	if !ok {
		return true, nil // as queueOwnerLive: a store with no identity rows has none to block
	}
	blocked, err := ib.IdentityBlocked(ctx, principal, -1)
	return err == nil && !blocked, err
}
