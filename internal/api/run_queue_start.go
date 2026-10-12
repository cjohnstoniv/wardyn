// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The start of a promoted run (pool-queue.md section 7). The run left the queue holding nothing; the
// start acts as its owner, with the authority the owner held at launch (queueAuthority), and re-checks
// everything that can have changed while it waited before it mints an identity or builds anything.
// It replays the create fold on the stored request, marked promoted so the capacity steps (already
// applied by the promotion) stand down, and then does what create does after its mint.

// queueStartPrepTimeout bounds the start's work before dispatch: the replay, the renewal, the mint and
// the grant writes. The image build and the dispatch that follow run on their own long deadlines.
const queueStartPrepTimeout = 2 * time.Minute

// queueTokenStartFailed marks a start that could not finish for a reason of ours, not the owner's: a
// store or provider that would not answer. The run is failed with a sentence and no queue reason.
const (
	queueStartFailedHint    = "This run left the queue, but could not be started. Start it again."
	queueStartRefusedToken  = "queue_start_refused"
	queueStartFailedLogName = "wardynd: queue start"
)

// promotedStartCtxKey marks a request context as the queue's start replaying a promoted run.
type promotedStartCtxKey struct{}

func withPromotedStart(ctx context.Context) context.Context {
	return context.WithValue(ctx, promotedStartCtxKey{}, true)
}

// startingPromoted reports whether ctx is a promoted run's start, whose capacity was admitted by the
// promotion; the fold reads it as runFold.promoted.
func startingPromoted(ctx context.Context) bool {
	v, _ := ctx.Value(promotedStartCtxKey{}).(bool)
	return v
}

// queueEnd is how a start that cannot go on ends the run: the queue reason (the status_detail token,
// "" for none), the sentence under the FAILED badge, and what the run.queue.start failure row adds.
type queueEnd struct {
	token string
	hint  string
	extra map[string]any
}

// queuedStart is one promoted run's start: the run and its queue row, the owner's identity, and the
// two contexts it works on. ctx is the owner's and a kill cancels it; base survives a kill, so the
// ending write and the audit rows still land.
type queuedStart struct {
	run    types.AgentRun
	entry  store.RunQueueEntry
	auth   queueAuthority
	launch queuedLaunch
	base   context.Context
	ctx    context.Context
	r      *http.Request
	cw     *queueRefusalCapture
	// daemon and prep are base and ctx before the owner's identity is put on them, so the start can put
	// a narrower one on in its place (queueCurrentRole).
	daemon, prep context.Context
	// f is the replayed fold, set once it has run.
	f runFold
}

// startQueuedRun starts one promoted run, in the process that promoted it, detached. It holds the
// run's watcher lease until dispatch takes its own, and registers the run with creates.track so a
// kill reaches it.
func (s *Server) startQueuedRun(base context.Context, p store.PromotedRun) {
	run := p.Run
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(base, "wardynd: queue start panicked", slog.String("run_id", run.ID.String()), slog.Any("panic", rec))
			s.failAndRevoke(base, run.ID, types.RunPending, createRunInternalError)
		}
	}()
	stopLease := s.holdRunWatcherLease(base, run.ID)
	defer stopLease()
	tracked, untrack := s.creates.track(base, run.ID)
	defer untrack()

	prep, cancel := context.WithTimeout(tracked, queueStartPrepTimeout)
	defer cancel()
	st, err := newQueuedStart(base, prep, p)
	if err != nil {
		s.endPromoted(st, &queueEnd{hint: queueStartFailedHint})
		return
	}
	if end := s.runQueuedStart(st); end != nil {
		s.endPromoted(st, end)
	}
}

// newQueuedStart reads the promoted run's queue row and builds the owner's context and the synthetic
// request the replayed gates read: the owner's identity with the ceiling and capability memos a request
// carries, so every read of the fold sees one answer. base survives a kill; prep is the context the
// replay and the mint work on. On an unreadable row it still returns the start, so the caller can end
// the run.
func newQueuedStart(base, prep context.Context, p store.PromotedRun) (*queuedStart, error) {
	st := &queuedStart{run: p.Run, entry: p.Entry, base: base, daemon: base, prep: prep, cw: newQueueRefusalCapture()}
	if err := json.Unmarshal(p.Entry.Launch, &st.launch); err != nil {
		return st, err
	}
	var auth queueAuthority
	_ = json.Unmarshal(p.Entry.Authority, &auth) //nolint:errcheck // an unreadable authority fails the owner checks
	return st, st.bind(auth)
}

// bind makes a the identity the start acts as: on its contexts and on the request the gates read.
func (st *queuedStart) bind(a queueAuthority) error {
	st.auth = a
	st.base = a.context(st.daemon)
	st.ctx = withPromotedStart(withCeilingMemo(withCapBatch(a.context(st.prep))))
	r, err := http.NewRequestWithContext(st.ctx, http.MethodPost, "/api/v1/runs", nil)
	if err != nil {
		return err
	}
	st.r = r
	return nil
}

// runQueuedStart is the table of section 7: the owner's standing, the profile, the doors, the policy,
// the pool and the fold, then the mint and the launch. It returns the end for the first refusal, nil
// when the run was handed to dispatch or was already taken by a kill.
func (s *Server) runQueuedStart(st *queuedStart) *queueEnd {
	if end := s.queueOwnerStanding(st); end != nil {
		return end
	}
	if end := s.queueCurrentRole(st); end != nil {
		return end
	}
	var req createRunRequest
	if err := json.Unmarshal(st.launch.Request, &req); err != nil {
		return &queueEnd{hint: queueStartFailedHint}
	}
	// Re-resolve the pool and the provider the run was admitted with, never the current defaults.
	req.RunnerPoolID = st.run.RunnerPoolID.String()
	if req.ModelProvider == "" {
		req.ModelProvider = st.launch.ProviderID
	}
	ceiling, end := s.queueGovernance(st, req)
	if end != nil {
		return end
	}
	if end := s.queuePolicyUnchanged(st, req); end != nil {
		return end
	}
	reqCC, refusal := runRequestEnums(req)
	if refusal.write(s, st.cw, st.r) {
		return st.refusalEnd()
	}
	if st.launch.ConfinementDefault != "" {
		// The launch named no class: it keeps the one the create resolved, not a fresh pick of the
		// organisation's default. The fold still holds that class to the floor and to what the pool offers.
		reqCC = st.run.ConfinementClass
	}
	f, ok := s.foldRunRequest(st.cw, st.r, foldCreate, &req, ceiling, reqCC)
	if !ok {
		return st.refusalEnd()
	}
	st.f = f
	if end := s.queueNarrowOnly(st); end != nil {
		return end
	}
	return s.queueLaunch(st, req, ceiling)
}

// refusalEnd turns the answer a replayed gate wrote into the run's end: the gate's reason is the token
// and its sentence is the hint, so the person reads what a refusal at create would have said.
func (st *queuedStart) refusalEnd() *queueEnd {
	reason, msg, _ := st.cw.refused()
	end := &queueEnd{token: reason, hint: msg}
	if reason == "" || queueToken(statusDetailQueuePrefix+reason) == "" {
		end.token = queueStartRefusedToken
	}
	if msg == "" {
		end.hint = queueStartFailedHint
	}
	return end
}

// queueOwnerStanding is step 1: the owner may still act as they did when the run was queued
// (queueOwnerLive), and a portal launch's portal is still registered. The portal is read from its row
// (delegates.revoked_at), never from the delegated token, whose ten-minute expiry says nothing about a
// run that waited longer. A check the store could not answer is asked again before the start gives up.
func (s *Server) queueOwnerStanding(st *queuedStart) *queueEnd {
	live, err := s.queueOwnerLive(st.ctx, st.auth, st.entry.QueuedAt)
	for try := 1; err != nil && try < queueOwnerCheckTries; try++ {
		select {
		case <-st.ctx.Done():
			return &queueEnd{hint: queueStartFailedHint}
		case <-time.After(time.Duration(try) * queueOwnerCheckBackoff):
		}
		live, err = s.queueOwnerLive(st.ctx, st.auth, st.entry.QueuedAt)
	}
	switch {
	case err != nil:
		slog.ErrorContext(st.base, queueStartFailedLogName+": check the owner", slog.String("run_id", st.run.ID.String()), slog.Any("err", err))
		return &queueEnd{hint: queueStartFailedHint}
	case !live:
		return &queueEnd{token: string(types.RunEndIdentityDeactivated), hint: types.RunEndIdentityDeactivated.Sentence()}
	}
	via, ok := st.auth.via()
	if !ok {
		return nil
	}
	dr, ok := s.cfg.Store.(store.DelegateReader)
	if !ok {
		return &queueEnd{hint: queueStartFailedHint}
	}
	d, err := dr.GetDelegateByID(st.ctx, via.Delegate)
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && d.RevokedAt != nil:
		return &queueEnd{token: string(types.RunEndQueuePortalRevoked), hint: types.RunEndQueuePortalRevoked.Sentence()}
	case err != nil:
		return &queueEnd{hint: queueStartFailedHint}
	}
	return nil
}

// queueGovernance is steps 2 and 3: the profile the owner resolves to now is the one they launched
// under, and the launch doors the run went through are still open to them. The owner's pool, run
// type and barrier are the fold's.
func (s *Server) queueGovernance(st *queuedStart, req createRunRequest) (governanceCeiling, *queueEnd) {
	cur, err := s.effectiveCeiling(st.ctx)
	if err != nil {
		ceilingRunRefusal(err).write(s, st.cw, st.r)
		return governanceCeiling{}, st.refusalEnd()
	}
	if !sameProfile(cur, st.launch.ProfileID) {
		return governanceCeiling{}, &queueEnd{token: string(types.RunEndQueueProfileChanged), hint: types.RunEndQueueProfileChanged.Sentence()}
	}
	ceiling, refusal := s.runRequestGovernance(st.r, req)
	if refusal.write(s, st.cw, st.r) {
		return governanceCeiling{}, st.refusalEnd()
	}
	if refusal := s.runGovernancePosture(st.r, req, ceiling, true); refusal.write(s, st.cw, st.r) {
		return governanceCeiling{}, st.refusalEnd()
	}
	if refusal := s.runGovernanceSupervision(st.r, req, ceiling); refusal.write(s, st.cw, st.r) {
		return governanceCeiling{}, st.refusalEnd()
	}
	ref, err := s.ownerCapabilityRefusal(st.ctx, st.run, true, st.launch.Repos)
	if err != nil {
		return governanceCeiling{}, &queueEnd{hint: queueStartFailedHint}
	}
	if ref != nil {
		return governanceCeiling{}, &queueEnd{token: ref.reason, hint: ref.msg}
	}
	return ceiling, nil
}

// sameProfile reports whether ceiling resolved to the profile the launch recorded (nil for none).
func sameProfile(ceiling governanceCeiling, recorded *uuid.UUID) bool {
	if ceiling.Profile == nil || recorded == nil {
		return ceiling.Profile == nil && recorded == nil
	}
	return ceiling.Profile.ID == *recorded
}

// queuePolicyUnchanged is step 5 (Q7-A): the stored policy or launch preset the run was reviewed under
// is the one it would start under. An edit fails the run, so nothing starts that nobody reviewed.
func (s *Server) queuePolicyUnchanged(st *queuedStart, req createRunRequest) *queueEnd {
	now, err := s.queuePolicyRef(st.ctx, req)
	if err != nil {
		return &queueEnd{hint: queueStartFailedHint}
	}
	if !jsonEqual(now, st.entry.PolicyRef) {
		return &queueEnd{token: string(types.RunEndQueuePolicyChanged), hint: types.RunEndQueuePolicyChanged.Sentence()}
	}
	return nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x) //nolint:errcheck // a value just unmarshalled marshals
	by, _ := json.Marshal(y) //nolint:errcheck // a value just unmarshalled marshals
	return string(ax) == string(by)
}

// queueNarrowOnly is invariant 4: the start only narrows. The replay may not choose a different model
// provider than the launch did, nor enforce a weaker barrier; either is the policy having changed.
func (s *Server) queueNarrowOnly(st *queuedStart) *queueEnd {
	if f := st.f; f.mpChoice.provider.ID != st.run.ModelProviderID || f.enforced.Rank() < st.run.ConfinementClass.Rank() {
		return &queueEnd{token: string(types.RunEndQueuePolicyChanged), hint: types.RunEndQueuePolicyChanged.Sentence()}
	}
	return nil
}

// queueLaunch is steps 6 to 9: bind a runner, recompute the end and its cap from the moment of
// admission, mint the identity, write what the QUEUED row left empty, wire the grants and hand the run
// to the launch tail create uses. After the mint it re-reads the state and revokes if a kill won.
func (s *Server) queueLaunch(st *queuedStart, req createRunRequest, ceiling governanceCeiling) *queueEnd {
	f := st.f
	qs, _ := s.queueStore()
	run := st.run
	placed := types.AgentRun{ID: run.ID}
	if ref := f.place.apply(&placed, f.enforced); ref.write(s, st.cw, st.r) {
		return st.refusalEnd()
	}
	// The end counts from admission, not from the launch: a run that waited an hour has its whole span.
	anchor := run
	anchor.CreatedAt = *run.AdmittedAt
	s.captureRunLimits(&anchor, ceiling, &f.end)

	id, err := s.cfg.Identity.MintRunIdentity(st.ctx, run.ID, runIdentitySubject(st.ctx, run.CreatedBy), requestSponsor(st.ctx, run.CreatedBy), internalAudience, run.OperatorOwned)
	if err != nil {
		slog.ErrorContext(st.base, queueStartFailedLogName+": mint run identity", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return &queueEnd{hint: queueStartFailedHint}
	}
	started, ok, err := qs.StartQueuedRun(st.ctx, run.ID, store.QueueStart{
		SPIFFEID: id.SPIFFEID, ConfinementClass: f.enforced, ModelProviderID: f.mpChoice.provider.ID, AutonomyLevel: f.autonomy.Level,
		EndsAt: anchor.EndsAt, WaitBudgetSec: anchor.WaitBudgetSec, RunLimits: anchor.RunLimits, GovernanceProfileID: anchor.GovernanceProfileID,
		LifetimeCapAt: anchor.LifetimeCapAt, LifetimeCapSource: anchor.LifetimeCapSource,
		Placement: placed.Placement, PlacementFilled: placed.PlacementFilled, RunnerID: placed.RunnerID,
		EvidenceSource: placed.EvidenceSource, RunnerPoolExecutor: placed.RunnerPoolExecutor,
	})
	switch {
	case errors.Is(err, store.ErrRunnerAtCapacity):
		// The start chose a runner other than the one the promotion found room on, and that one is full.
		return &queueEnd{token: string(placement.ReasonRunnerAtCapacity), hint: runnerpool.RunnerAtCapacityMsg(f.place.pool.Name)}
	case err != nil:
		slog.ErrorContext(st.base, queueStartFailedLogName+": write start columns", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return &queueEnd{hint: queueStartFailedHint}
	case !ok:
		s.revokeKilledStart(st)
		return nil
	}
	gw, _, ok := s.wireRunGrants(st.ctx, st.cw, st.r, started, s.cfg.Now().UTC(), f.spec, req, f.comps, f.localDelivery)
	if !ok {
		return &queueEnd{hint: createRunGrantAbortHint}
	}
	if cur, gerr := s.cfg.Store.GetRun(st.base, run.ID); gerr != nil || cur.State != types.RunPending {
		s.revokeKilledStart(st)
		return nil
	}
	s.auditQueueStart(st, started, anchor, id.JTI)
	spec := f.spec
	s.unionRunEgress(st.base, run.ID, &spec, gw, f.wsRefs, req.Repo, f.scmSite, f.directGitHubAdded, f.comps)
	spec, harnessRules := applyHarnessToolRules(spec, req.Tools, toolLaneDefault(req))
	s.metrics.queueWait(queueOutcomeStarted, run.AdmittedAt.Sub(st.entry.QueuedAt))
	s.finishCreateRunLaunch(st.base, createRunLaunch{
		req: req, spec: spec, ceiling: ceilingForDispatch(ceiling, f.adoGrade, f.bedrockGrade), gw: gw,
		wsRefs: f.wsRefs, driveMounts: f.driveMounts, ephemeralDirs: f.ephemeralDirs,
		runToken: id.Token, created: started, comps: f.comps, overridden: f.source.overridden(), pushSets: f.pushSets, harnessRules: harnessRules,
	})
	return nil
}

// revokeKilledStart revokes what the start minted when a kill won the run first: the identity and
// anything the grants wired. The kill's own cascade ran before the mint, so it could not.
func (s *Server) revokeKilledStart(st *queuedStart) {
	if err := s.revokeRunCascade(st.base, st.run.ID); err != nil {
		slog.WarnContext(st.base, queueStartFailedLogName+": revoke after a kill", slog.String("run_id", st.run.ID.String()), slog.Any("err", err))
	}
}

// auditQueueStart is the run.queue.start success row: how long the run waited, the pool's slots, the
// limits it started under, the CPU and memory it got as the replayed placement applied them against the
// limits as they stand at the start (resources_applied, run.create's shape), and the jti of its identity;
// a portal launch's row carries via.
func (s *Server) auditQueueStart(st *queuedStart, started, anchor types.AgentRun, jti string) {
	data := map[string]any{
		"waited_sec": int(started.AdmittedAt.Sub(st.entry.QueuedAt).Seconds()), "pool_id": st.entry.PoolID, "jti": jti,
		"limits":            map[string]any{"ends_at": anchor.EndsAt, "lifetime_cap_at": anchor.LifetimeCapAt, "run_limits": anchor.RunLimits},
		"resources_applied": st.f.place.applied,
		// The class the run started under: the one its create resolved, whoever chose it then.
		"confinement_class": started.ConfinementClass,
	}
	if st.launch.ConfinementDefault != "" {
		data["confinement_default"] = st.launch.ConfinementDefault
	}
	if c, ok := s.cfg.Store.(interface {
		CountRunnerPoolSlotHolders(ctx context.Context, poolID uuid.UUID) (int, error)
	}); ok {
		if n, err := c.CountRunnerPoolSlotHolders(st.base, st.entry.PoolID); err == nil {
			data["slots"] = map[string]any{"used": n}
		}
	}
	s.recordAudit(st.base, s.auditEvent(&started.ID, types.ActorSystem, "wardynd", "run.queue.start", started.ID.String(), "success", mustJSON(data)))
}

// endPromoted fails a promoted run whose start cannot go on: the reason lands in status_detail before
// the transition, the sentence in failure_hint, the identity (if any was minted) is revoked, the
// failure is audited and the metric counted. The slot it held frees, and casRunState kicks the queue.
func (s *Server) endPromoted(st *queuedStart, end *queueEnd) {
	run := st.run
	if end.token != "" {
		if qs, ok := s.queueStore(); ok {
			if err := qs.SetPromotedStatusDetail(st.base, run.ID, statusDetailQueuePrefix+end.token); err != nil {
				slog.WarnContext(st.base, queueStartFailedLogName+": record end reason", slog.String("run_id", run.ID.String()), slog.Any("err", err))
			}
		}
	}
	if !s.failAndRevoke(st.base, run.ID, types.RunPending, end.hint) {
		return // a kill or another start's end moved the run first, and wrote its own rows
	}
	data := map[string]any{"reason": end.token, "pool_id": st.entry.PoolID}
	for k, v := range end.extra {
		data[k] = v
	}
	if end.token == "" {
		data["reason"] = queueStartRefusedToken
	}
	s.metrics.queueWait(queueOutcomeRefused, run.AdmittedAt.Sub(st.entry.QueuedAt))
	s.recordAudit(st.base, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.queue.start", run.ID.String(), "failure", mustJSON(data)))
}
