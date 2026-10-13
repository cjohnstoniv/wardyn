// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runner revoke doors. Neither is gated on runners.enabled: cutting a
// runner off must stay possible while runners are switched off.
//
//	DELETE /runners/{id}                       security (admin or security_admin)
//	DELETE /me/runners/{id}[?reason=not_mine]  the runner's owner, personally signed in
func (s *Server) mountRunnerRevokeRoutes(r, securityOps chi.Router) {
	securityOps.Delete("/runners/{id}", s.handleRevokeRunner)
	r.Delete("/me/runners/{id}", s.handleRevokeMyRunner)
}

func (s *Server) handleRevokeRunner(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	s.revokeRunnerAndAnswer(w, r, id, "")
}

// handleRevokeMyRunner answers an absent runner, another person's, a lapsed
// unclaimed one and an already revoked one with the same 404.
func (s *Server) handleRevokeMyRunner(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason != "" && reason != types.RunnerRevokeNotMine {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerRevokeReasonInvalid, "reason must be empty or not_mine")
		return
	}
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, msgRunnerRevokeUnavailable)
		return
	}
	row, err := st.GetRunner(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "get runner", err)
		return
	}
	if err != nil || row.Owner != owner || row.State == types.RunnerRevoked || row.ClaimExpired(s.cfg.Now()) {
		writeErrorReason(w, http.StatusNotFound, string(placement.ReasonRunnerNotFound), "runner not found")
		return
	}
	s.revokeRunnerAndAnswer(w, r, id, reason)
}

func (s *Server) revokeRunnerAndAnswer(w http.ResponseWriter, r *http.Request, id uuid.UUID, reason string) {
	actorType, by := actorFromRequest(r)
	rev, queued, err := s.revokeRunner(context.WithoutCancel(r.Context()), id, actorType, by, reason)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, string(placement.ReasonRunnerNotFound), "runner not found")
		return
	case errors.Is(err, errRunnerRevokeUnavailable):
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, msgRunnerRevokeUnavailable)
		return
	case rev.Runner.ID == uuid.Nil:
		writeServerError(w, r, "revoke runner", err)
		return
	}
	body := map[string]any{"id": id, "state": types.RunnerRevoked, "runs_killed": len(rev.Killed) + queued}
	if err != nil {
		body["error"] = "runner revoked, but closing its session or ending a run on it did not complete; the kill-tail sweep retries the run steps"
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

const msgRunnerRevokeUnavailable = "runner revocation requires the Postgres runner store"

var errRunnerRevokeUnavailable = errors.New(msgRunnerRevokeUnavailable)

// revokeRunner is the whole revoke. The runner is unrouted first, so no new
// teardown queues against it, and re-routed if the revoke does not commit.
// One transaction revokes it, ends its runs KILLED (RN-Q29) and settles its
// pending actions. After that commit: the runs waiting in a pool's queue for
// this runner alone are ended too (queued, how many), runner.revoke is audited,
// the session is closed (Hub.Revoke takes the session slot, so never under a
// database lock), and each killed run gets the kill cascade. A result naming the
// runner means the revoke committed, even when err reports an incomplete cascade.
func (s *Server) revokeRunner(ctx context.Context, id uuid.UUID, actorType types.ActorType, by, reason string) (rev store.RunnerRevocation, queued int, err error) {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return store.RunnerRevocation{}, 0, errRunnerRevokeUnavailable
	}
	s.unrouteRunner(id)
	rev, err = st.RevokeRunner(ctx, id, s.cfg.Now(), by, reason)
	if err != nil {
		if cur, gerr := st.GetRunner(context.WithoutCancel(ctx), id); gerr == nil && cur.State == types.RunnerClaimed {
			s.runnerEndpoint(id)
		}
		return store.RunnerRevocation{}, 0, err
	}
	// An admission racing the revoke may have re-registered the endpoint.
	s.unrouteRunner(id)
	queued, qerr := s.endQueuedRunsPinnedTo(ctx, rev.Runner.Owner, id)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, by, placement.ActionRunnerRevoke, id.String(), "success",
		mustJSON(map[string]any{"by": by, "reason": reason, "runs_killed": len(rev.Killed) + queued})))
	online := s.RunnerOnline(id)
	errs := []error{qerr, s.RevokeRunnerSession(ctx, id)}
	for _, k := range rev.Killed {
		errs = append(errs, s.revokedRunKillTail(ctx, k, actorType, by, online))
	}
	return rev, queued, errors.Join(errs...)
}

// endQueuedRunsPinnedTo ends, as the system, each run owner has waiting in a pool's queue that names
// runner id (runner_id), which can never start once id is revoked, with reason runner_revoked on its
// run.kill row, and returns how many it ended. The owner's other waiting runs are judged again at once,
// without the runner. A run another exit takes first is not counted. One that cannot be read or ended
// does not keep the rest waiting: its error is returned with the others'.
func (s *Server) endQueuedRunsPinnedTo(ctx context.Context, owner string, id uuid.UUID) (int, error) {
	qs, ok := s.queueStore()
	if !ok {
		return 0, nil
	}
	entries, err := qs.QueueOwnerEntries(ctx, owner)
	if err != nil {
		return 0, err
	}
	defer s.kickQueue()
	n := 0
	var errs []error
	for _, e := range entries {
		var launch queuedLaunch
		var req createRunRequest
		if json.Unmarshal(e.Launch, &launch) != nil || json.Unmarshal(launch.Request, &req) != nil || req.RunnerID != id.String() {
			continue
		}
		run, err := s.cfg.Store.GetRun(ctx, e.RunID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		applied, _, err := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": queueCancelRunnerRevoked})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if applied {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// revokedRunKillTail is the kill cascade of a run its runner's revocation
// ended KILLED: the transition's own effects, the run's approvals, then
// killTeardownTail without a sandbox ref. The org cannot reach a revoked
// runner; the runner stops the run's network itself when told REVOKED.
func (s *Server) revokedRunKillTail(ctx context.Context, k store.RunnerKilledRun, actorType types.ActorType, by string, online bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	s.metrics.runTerminal(types.RunKilled)
	s.runEvents.moved(k.ID, k.From, types.RunKilled)
	s.cancelCreate(k.ID)
	s.cancelRunApprovals(ctx, k.ID)
	killData := s.killTeardownTail(ctx, types.AgentRun{ID: k.ID}, actorType, by,
		map[string]any{"reason": string(placement.ReasonRunnerRevoked), "runner_online": online}, false)
	if len(killData) == 0 {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(&k.ID, types.ActorSystem, "wardynd", "run.revoke",
		k.ID.String(), "failure", mustJSON(killData)))
	return errors.New("run kill cascade incomplete for " + k.ID.String())
}

// unrouteRunner forgets the runner's endpoint and removes it from the orchestrator.
func (s *Server) unrouteRunner(id uuid.UUID) {
	s.runnerStreams.subsMu.Lock()
	ep, ok := s.runnerStreams.subs[id]
	delete(s.runnerStreams.subs, id)
	s.runnerStreams.subsMu.Unlock()
	if reg, isReg := s.cfg.Runner.(substrateRegistry); ok && isReg {
		reg.Remove(ep.sub)
	}
}
