// Copyright 2026 The Wardyn Authors
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
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Promotion: the oldest waiting run of a pool that can be placed moves from QUEUED to PENDING when the
// pool has a free slot, once, under the pool row's lock (store.PromoteRunQueue). The promoting process
// then starts it (run_queue_start.go). Anyone may promote: a create after its commit, any replica after
// a commit that frees a slot, and the leader's sweep every ten seconds. Correctness rests on the pool
// lock and the state CAS, never on who asks.

// promoteQueue runs one bounded promotion pass for a pool and starts what it promoted, detached. On the
// bound the transaction rolls back and the runs stay QUEUED for the next pass. ctx is the caller's, for
// the log lines only. The error is what the pass could not do (a failed read or write, or the bound); a
// pool at capacity, or a run that cannot be placed yet, is a wait and not an error.
func (s *Server) promoteQueue(ctx context.Context, poolID uuid.UUID, bound time.Duration) error {
	qs, ok := s.queueStore()
	if !ok {
		return nil
	}
	// The pass judges other people's runs, so it works on the daemon's own context: a caller's identity
	// on ctx must never reach it.
	pctx, cancel := context.WithTimeout(inheritQueuePass(context.WithoutCancel(s.cfg.BaseCtx), ctx), bound)
	defer cancel()
	promoted, held, err := qs.PromoteRunQueue(pctx, store.PromotePass{
		PoolID: poolID, DeploymentCap: s.cfg.MaxConcurrentRuns, ScanLimit: queueScanLimit, WatcherOwner: watcherOwner,
		AtCapacityDetail: statusDetailQueuePrefix + string(runnerpool.WaitAtCapacity),
	}, s.queueAdmit)
	var errs []error
	switch {
	case err != nil && len(promoted) == 0:
		slog.WarnContext(ctx, "wardynd: queue promotion deferred", slog.String("pool_id", poolID.String()), slog.Any("err", err))
		return fmt.Errorf("promote the queue of pool %s: %w", poolID, err)
	case err != nil:
		slog.WarnContext(ctx, "wardynd: queue promotion judged part of the queue", slog.String("pool_id", poolID.String()),
			slog.Int("promoted", len(promoted)), slog.Any("err", err))
		errs = append(errs, fmt.Errorf("promote the queue of pool %s: %w", poolID, err))
	}
	// Reason precedence for a waiting run: the pool's cap (runner_pool_at_capacity) outranks a reason
	// found while judging, because the judging held no lock and the pool may have filled since. The store
	// drops these writes when the pool is full by the time it writes them.
	details := map[uuid.UUID]string{}
	for _, h := range held {
		if h.Wait != "" {
			details[h.RunID] = statusDetailQueuePrefix + h.Wait
		}
	}
	if len(details) > 0 {
		if err := qs.SetHeldQueueWaits(ctx, poolID, details); err != nil {
			slog.WarnContext(ctx, "wardynd: record queue waits", slog.String("pool_id", poolID.String()), slog.Any("err", err))
			errs = append(errs, fmt.Errorf("record queue waits of pool %s: %w", poolID, err))
		}
	}
	for _, p := range promoted {
		s.runEvents.moved(p.Run.ID, types.RunQueued, types.RunPending)
		s.goBackground(func() { s.startQueuedRun(context.WithoutCancel(s.cfg.BaseCtx), p) })
	}
	return errors.Join(errs...)
}

// queueAdmit is the verdict on one waiting run, asked before the pass takes any lock: the run must be
// placeable on a runner of its owner now, and the host and the quota must have room for it. A run that is
// not keeps its place; a host with no room stops the pass. Whether the owner may still launch at all is
// not asked here: the start re-checks that and ends the run with a reason, rather than letting it wait
// for ever behind a refusal. An error is a read that failed, and the run keeps its place.
func (s *Server) queueAdmit(ctx context.Context, pool types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
	var launch queuedLaunch
	if err := json.Unmarshal(e.Launch, &launch); err != nil {
		// Nothing to judge it by: let the start end it with a reason.
		return store.QueueVerdict{Admit: true}, nil
	}
	var auth queueAuthority
	_ = json.Unmarshal(e.Authority, &auth) //nolint:errcheck // an unreadable authority fails the start's own checks
	room, err := s.queueRunnerFree(withQueueOwner(auth.context(ctx), e.Owner, e.Authority), pool, launch)
	if err != nil {
		return store.QueueVerdict{}, err
	}
	if room.wait != "" {
		// Only an unpinned run's wait is its owner's: a run pinned to one runner waits for that runner alone.
		return store.QueueVerdict{Wait: string(room.wait), Owner: !room.pinned}, nil
	}
	if s.admitHostCapacity(ctx, e.Owner, "runs", false) != nil {
		return store.QueueVerdict{Stop: true}, nil
	}
	// The launch keeps no model-provider choice, so an Azure-gated run is asked about with the default
	// proxy envelope here; its create was asked with the Azure one, and the quota's admission decides.
	if s.runFit(ctx, types.RunPolicySpec{Resources: launch.Resources}, time.Duration(launch.LifetimeCapSec)*time.Second, false, false).refusal != "" {
		return store.QueueVerdict{}, nil
	}
	return store.QueueVerdict{Admit: true, Runners: room.runners, Remotes: room.remotes, Needs: room.needs}, nil
}

// queueRoom is queueRunnerFree's answer: why the run cannot start now ("" when it can), whether it names
// its runner, and the runners it may be bound to, each online with room as last read: the owner's own
// runners, and the pool's remote-hosted runners.
type queueRoom struct {
	wait    runnerpool.WaitReason
	pinned  bool
	runners []uuid.UUID
	remotes []string
	// needs is the size the run would start with on each of those runners, by runner key; empty for a
	// launch queued before its unbound size was recorded.
	needs map[string]types.RunnerAmounts
}

// queueRunnerFree reports whether one of the owner's candidate targets in the pool can take a run now.
// The candidates are the ones the start's fold would keep, by the fold's own filters, each then held to
// room for the size the run would start with there. A pool the owner can no longer see, or a refusal of
// the owner's own (a user type that is gone), counts as free, so the start ends the run with its reason
// instead of leaving it to wait. Only a read that failed is an error. pinned says the launch names its
// runner (runner_id): only that runner is asked.
func (s *Server) queueRunnerFree(ctx context.Context, pool types.RunnerPool, launch queuedLaunch) (queueRoom, error) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return queueRoom{}, nil
	}
	var req createRunRequest
	_ = json.Unmarshal(launch.Request, &req) //nolint:errcheck // an unreadable request pins no runner
	out := queueRoom{pinned: req.RunnerID != ""}
	rows, refusal := s.queuePermittedPools(ctx, ps)
	if refusal != nil {
		if refusal.err != nil && !errors.Is(refusal.err, errUserTypeUnknown) {
			return out, fmt.Errorf("read the owner's pools: %w", refusal.err)
		}
		return out, nil
	}
	row, found := catalogueRow(rows, pool.ID)
	if !found {
		return out, nil
	}
	// The candidates are the start's own: the same targets, the same kinds (kindTargets: the kind asked
	// for, none the profile denies, no runner of the person's for a run with drives) and the same demand
	// on each (placementDemand.refusal: online, the barrier the run was queued at, limits declared, recording
	// under a required setting). A ceiling that cannot be read denies nothing here: the start judges the
	// run against it again.
	in := placementInput{req: &req, reqCC: launch.Barrier, reqCCOK: launch.Barrier != "", drives: launch.Drives, noLocal: launch.NoLocal}
	if ceiling, err := s.queueCeiling(ctx); err == nil {
		in.ceiling = ceiling
	}
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	if err != nil {
		return out, fmt.Errorf("read the organisation's runner pool defaults: %w", err)
	}
	requireRecording := org.Recording == types.RecordingRequired || row.Pool.Recording == types.RecordingRequired
	// A required setting with no store has no room anywhere, and no runner coming online changes that, so
	// it is said before any runner is looked at: the start would refuse recording_store_off.
	if requireRecording && s.cfg.RecordingStore == nil {
		out.wait = runnerpool.WaitRecordingStoreOff
		return out, nil
	}
	targets, refusal := s.poolTargets(ctx, &req, ps, row)
	if refusal != nil && refusal.err != nil {
		// A read that failed says nothing about the runners: the run keeps its place with no wait recorded.
		return out, fmt.Errorf("read the pool's runners: %w", refusal.err)
	}
	if refusal != nil {
		out.wait = runnerpool.WaitNoRunnerFree
		return out, nil
	}
	kinds, refusal := s.kindTargets(ctx, in, row.Pool, targets)
	if refusal != nil {
		out.wait = runnerpool.WaitNoRunnerFree
		return out, nil
	}
	// Room is read here without a lock, by each runner's declared limits and In use, for the size the
	// run would start with on that runner; the promotion holds that size to it again under the runner
	// rows' locks, the person's own runners first.
	fits, err := s.runnerFits(ctx)
	if err != nil {
		return out, err
	}
	demand := placementDemand{reqCC: in.reqCC, reqCCOK: in.reqCCOK, fits: fits,
		requireRecording: requireRecording}
	out.needs = map[string]types.RunnerAmounts{}
	online := false
	for _, t := range slices.Concat(kinds[types.PlacementLocal], kinds[types.PlacementRemote]) {
		if demand.refusal(t) != "" || (out.pinned && t.id != req.RunnerID) {
			continue
		}
		online = true
		f := demand.fits[t.id]
		var need types.RunnerAmounts
		if launch.Unbound != nil && f.limits != nil {
			need = startSizeOn(*launch.Unbound, f.limits)
		}
		if t.full || runnerpool.Fits(f.limits, f.used, need) != runnerpool.FitOK {
			continue
		}
		if launch.Unbound != nil {
			out.needs[t.id] = need
		}
		if t.runnerID == nil {
			out.remotes = append(out.remotes, t.executor)
		} else {
			out.runners = append(out.runners, *t.runnerID)
		}
	}
	switch {
	case !online:
		out.wait = runnerpool.WaitNoRunnerFree
	case len(out.runners)+len(out.remotes) == 0:
		out.wait = runnerpool.WaitRunnersAtCapacity
	}
	return out, nil
}

// kickQueue asks every pool with waiting runs to promote, off the caller's path. Concurrent kicks
// coalesce: a kick that finds one running leaves a note for it to go round once more.
func (s *Server) kickQueue() {
	if _, ok := s.queueStore(); !ok {
		return
	}
	if !s.queue.kick.start() {
		return
	}
	s.goBackground(func() {
		for {
			_ = s.promoteWaitingPools(s.cfg.BaseCtx) // logged there; the leader sweep reports it to sweep health
			if !s.queue.kick.again() {
				return
			}
		}
	})
}

// promoteWaitingPools runs one promotion pass for every pool that has waiting runs.
func (s *Server) promoteWaitingPools(ctx context.Context) error {
	qs, ok := s.queueStore()
	if !ok {
		return nil
	}
	ctx = withQueuePass(ctx)
	pools, err := qs.QueuePoolIDs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: list pools with waiting runs", slog.Any("err", err))
		return fmt.Errorf("list pools with waiting runs: %w", err)
	}
	var errs []error
	for _, id := range pools {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, s.promoteQueue(ctx, id, queueSweepBound))
	}
	return errors.Join(errs...)
}

// SweepRunQueue is one tick of the leader's queue sweep: end the waiting runs whose deadline passed and
// the ones whose named runner is revoked, fail the promoted runs whose start died, and promote for every
// pool that has waiting runs (a slot may have freed with no commit this replica saw). Every write is a
// CAS under the pool lock, so a second leader for a moment is harmless. A list read or a per-run write
// that fails is returned (joined), so the sweeper's health shows it; the other rows are still done.
func (s *Server) SweepRunQueue(ctx context.Context) error {
	if _, ok := s.queueStore(); !ok {
		return nil
	}
	return errors.Join(
		s.expireQueued(ctx),
		s.endQueuedRunsOfRevokedRunners(ctx),
		s.failStalePromoted(ctx),
		s.promoteWaitingPools(ctx),
	)
}

// expireQueued ends each waiting run past its deadline: FAILED queue_timeout, with run.queue.expire.
func (s *Server) expireQueued(ctx context.Context) error {
	qs, _ := s.queueStore()
	now := s.cfg.Now().UTC()
	expired, err := qs.ExpiredQueueEntries(ctx, now, 200)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: list expired queue entries", slog.Any("err", err))
		return fmt.Errorf("list expired queue entries: %w", err)
	}
	var errs []error
	for _, e := range expired {
		reason := types.RunEndQueueTimeout
		applied, err := s.exitQueued(ctx, e.RunID, types.RunFailed, statusDetailQueuePrefix+string(reason), reason.Sentence())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !applied {
			continue
		}
		waited := now.Sub(e.QueuedAt)
		s.metrics.queueWait(queueOutcomeTimeout, waited)
		s.recordAudit(ctx, s.auditEvent(&e.RunID, types.ActorSystem, "wardynd", "run.queue.expire", e.RunID.String(), "success",
			mustJSON(map[string]any{"waited_sec": int(waited.Seconds()), "deadline_at": e.DeadlineAt, "pool_id": e.PoolID})))
	}
	return errors.Join(errs...)
}

// queueLeaseStale is how long a promoted run's start may go without touching its lease before the
// process that promoted it is presumed dead: three missed heartbeats, the watcher lease's own window.
const queueLeaseStale = watcherLeaseStaleAfter

// failStalePromoted fails each promoted run (PENDING, no sandbox, admitted_at set) whose start stopped
// touching its lease, revoking any identity it was given, so its slot frees within a tick. A plain
// PENDING run in its image build is never listed: only promotion writes admitted_at.
func (s *Server) failStalePromoted(ctx context.Context) error {
	qs, _ := s.queueStore()
	runs, err := qs.StalePromotedRuns(ctx, queueLeaseStale)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: list stale promoted runs", slog.Any("err", err))
		return fmt.Errorf("list stale promoted runs: %w", err)
	}
	var errs []error
	for _, run := range runs {
		if !s.failAndRevoke(ctx, run.ID, types.RunPending, queueStartLostHint) {
			// false is also "a concurrent kill or start won", which is no failure: only a run still PENDING
			// (or unreadable) means the transition itself failed, and failAndRevoke has only logged that.
			switch cur, gerr := s.cfg.Store.GetRun(ctx, run.ID); {
			case gerr != nil:
				errs = append(errs, fmt.Errorf("read the stale promoted run %s: %w", run.ID, gerr))
			case cur.State == types.RunPending:
				errs = append(errs, fmt.Errorf("fail the stale promoted run %s: the transition did not apply", run.ID))
			}
			continue
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.queue.start", run.ID.String(), "failure",
			mustJSON(map[string]any{"reason": "start_lost", "pool_id": run.RunnerPoolID})))
	}
	return errors.Join(errs...)
}

// queueStartLostHint is the line under the FAILED badge of a promoted run whose start died.
const queueStartLostHint = "This run left the queue, but the server that was starting it stopped before it came up. Start it again."

// queueRefusalCapture is the ResponseWriter a replayed fold writes its refusal to: the start has no
// client to answer, so the refusal becomes the run's end reason instead.
type queueRefusalCapture struct {
	header http.Header
	status int
	body   []byte
}

func newQueueRefusalCapture() *queueRefusalCapture {
	return &queueRefusalCapture{header: http.Header{}}
}

func (c *queueRefusalCapture) Header() http.Header { return c.header }
func (c *queueRefusalCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *queueRefusalCapture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.body = append(c.body, b...)
	return len(b), nil
}

// refused reports whether the fold wrote an answer, and the reason and sentence it carried.
func (c *queueRefusalCapture) refused() (reason, msg string, ok bool) {
	if c.status == 0 {
		return "", "", false
	}
	var body errorBody
	_ = json.Unmarshal(c.body, &body) //nolint:errcheck // a refusal with no readable body is still a refusal
	return body.Reason, body.Error, true
}
