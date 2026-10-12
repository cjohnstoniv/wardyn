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
// the log lines only.
func (s *Server) promoteQueue(ctx context.Context, poolID uuid.UUID, bound time.Duration) {
	qs, ok := s.queueStore()
	if !ok {
		return
	}
	// The pass judges other people's runs, so it works on the daemon's own context: a caller's identity
	// on ctx must never reach it.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), bound)
	defer cancel()
	promoted, held, err := qs.PromoteRunQueue(pctx, store.PromotePass{
		PoolID: poolID, DeploymentCap: s.cfg.MaxConcurrentRuns, ScanLimit: queueScanLimit, WatcherOwner: watcherOwner,
		AtCapacityDetail: statusDetailQueuePrefix + string(runnerpool.WaitAtCapacity),
	}, s.queueAdmit)
	switch {
	case err != nil && len(promoted) == 0:
		slog.WarnContext(ctx, "wardynd: queue promotion deferred", slog.String("pool_id", poolID.String()), slog.Any("err", err))
		return
	case err != nil:
		slog.WarnContext(ctx, "wardynd: queue promotion judged part of the queue", slog.String("pool_id", poolID.String()),
			slog.Int("promoted", len(promoted)), slog.Any("err", err))
	}
	for _, h := range held {
		if h.Wait == "" {
			continue
		}
		if err := qs.SetQueueWait(ctx, h.RunID, statusDetailQueuePrefix+h.Wait); err != nil {
			slog.WarnContext(ctx, "wardynd: record queue wait", slog.String("run_id", h.RunID.String()), slog.Any("err", err))
		}
	}
	for _, p := range promoted {
		s.runEvents.moved(p.Run.ID, types.RunQueued, types.RunPending)
		s.goBackground(func() { s.startQueuedRun(context.WithoutCancel(s.cfg.BaseCtx), p) })
	}
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
	free, pinned, err := s.queueRunnerFree(auth.context(ctx), pool, launch)
	if err != nil {
		return store.QueueVerdict{}, err
	}
	if !free {
		// Only an unpinned run's wait is its owner's: a run pinned to one runner waits for that runner alone.
		return store.QueueVerdict{Wait: string(runnerpool.WaitNoRunnerFree), Owner: !pinned}, nil
	}
	if s.admitHostCapacity(ctx, e.Owner, "runs", false) != nil {
		return store.QueueVerdict{Stop: true}, nil
	}
	if s.runFit(ctx, types.RunPolicySpec{Resources: launch.Resources}, time.Duration(launch.LifetimeCapSec)*time.Second, false).refusal != "" {
		return store.QueueVerdict{}, nil
	}
	return store.QueueVerdict{Admit: true}, nil
}

// queueRunnerFree reports whether one of the owner's candidate targets in the pool can take a run now:
// at least one is online with known facts. A pool the owner can no longer see, or a refusal of the
// owner's own (a user type that is gone), counts as free, so the start ends the run with its reason
// instead of leaving it to wait. Only a read that failed is an error. pinned says the launch names its
// runner (runner_id): on a pool of the owner's own runners only that runner is asked.
func (s *Server) queueRunnerFree(ctx context.Context, pool types.RunnerPool, launch queuedLaunch) (free, pinned bool, err error) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return true, false, nil
	}
	var req createRunRequest
	_ = json.Unmarshal(launch.Request, &req) //nolint:errcheck // an unreadable request pins no runner
	pinned = req.RunnerID != ""
	rows, refusal := s.permittedPools(ctx, ps, true)
	if refusal != nil {
		if refusal.err != nil && !errors.Is(refusal.err, errUserTypeUnknown) {
			return false, pinned, fmt.Errorf("read the owner's pools: %w", refusal.err)
		}
		return true, pinned, nil
	}
	row, found := catalogueRow(rows, pool.ID)
	if !found {
		return true, pinned, nil
	}
	targets, refusal := s.poolTargets(ctx, &req, ps, row)
	if refusal != nil {
		return false, pinned, nil
	}
	for _, t := range targets {
		if !t.offline && (!pinned || pool.HostingType != types.RunnerPoolSelfHosted || t.id == req.RunnerID) {
			return true, pinned, nil
		}
	}
	return false, pinned, nil
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
			s.promoteWaitingPools(s.cfg.BaseCtx)
			if !s.queue.kick.again() {
				return
			}
		}
	})
}

// promoteWaitingPools runs one promotion pass for every pool that has waiting runs.
func (s *Server) promoteWaitingPools(ctx context.Context) {
	qs, ok := s.queueStore()
	if !ok {
		return
	}
	pools, err := qs.QueuePoolIDs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: list pools with waiting runs", slog.Any("err", err))
		return
	}
	for _, id := range pools {
		if ctx.Err() != nil {
			return
		}
		s.promoteQueue(ctx, id, queueSweepBound)
	}
}

// SweepRunQueue is one tick of the leader's queue sweep: end the waiting runs whose deadline passed,
// fail the promoted runs whose start died, and promote for every pool that has waiting runs (a slot may
// have freed with no commit this replica saw). Every write is a CAS under the pool lock, so a second
// leader for a moment is harmless.
func (s *Server) SweepRunQueue(ctx context.Context) error {
	if _, ok := s.queueStore(); !ok {
		return nil
	}
	s.expireQueued(ctx)
	s.failStalePromoted(ctx)
	s.promoteWaitingPools(ctx)
	return nil
}

// expireQueued ends each waiting run past its deadline: FAILED queue_timeout, with run.queue.expire.
func (s *Server) expireQueued(ctx context.Context) {
	qs, _ := s.queueStore()
	now := s.cfg.Now().UTC()
	expired, err := qs.ExpiredQueueEntries(ctx, now, 200)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: list expired queue entries", slog.Any("err", err))
		return
	}
	for _, e := range expired {
		reason := types.RunEndQueueTimeout
		if !s.exitQueued(ctx, e.RunID, types.RunFailed, statusDetailQueuePrefix+string(reason), reason.Sentence()) {
			continue
		}
		waited := now.Sub(e.QueuedAt)
		s.metrics.queueWait(queueOutcomeTimeout, waited)
		s.recordAudit(ctx, s.auditEvent(&e.RunID, types.ActorSystem, "wardynd", "run.queue.expire", e.RunID.String(), "success",
			mustJSON(map[string]any{"waited_sec": int(waited.Seconds()), "deadline_at": e.DeadlineAt, "pool_id": e.PoolID})))
	}
}

// queueLeaseStale is how long a promoted run's start may go without touching its lease before the
// process that promoted it is presumed dead: three missed heartbeats, the watcher lease's own window.
const queueLeaseStale = watcherLeaseStaleAfter

// failStalePromoted fails each promoted run (PENDING, no sandbox, admitted_at set) whose start stopped
// touching its lease, revoking any identity it was given, so its slot frees within a tick. A plain
// PENDING run in its image build is never listed: only promotion writes admitted_at.
func (s *Server) failStalePromoted(ctx context.Context) {
	qs, _ := s.queueStore()
	runs, err := qs.StalePromotedRuns(ctx, queueLeaseStale)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: list stale promoted runs", slog.Any("err", err))
		return
	}
	for _, run := range runs {
		if !s.failAndRevoke(ctx, run.ID, types.RunPending, queueStartLostHint) {
			continue
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.queue.start", run.ID.String(), "failure",
			mustJSON(map[string]any{"reason": "start_lost", "pool_id": run.RunnerPoolID})))
	}
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
