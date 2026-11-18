// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// errRunnerEventShape marks a handler's refusal of an event's own shape, before any store call; each kind's
// shape error also carries the sentinel that kind refused with before.
var (
	errRunnerEventShape  = errors.New("runner event shape refused")
	errActionResultShape = fmt.Errorf("%w: %w", errRunnerEventShape, store.ErrRunnerActionRefused)
	errAgentExitShape    = fmt.Errorf("%w: %w", errRunnerEventShape, store.ErrRunnerExecutionRefused)
	errSpoolShape        = fmt.Errorf("%w: %w", errRunnerEventShape, store.ErrRunnerSpoolRefused)
	errReportShape       = fmt.Errorf("%w: %w", errRunnerEventShape, errRunnerEventRefused)

	errWorkloadEventRateLimited = errors.New("workload events past this runner's rate")
)

// workloadEventRatePerSec and workloadEventBurst bound one runner's workload events. The burst is what the rate earns
// between two full reports (cmd/wardyn-runnerd's workloadResync, 10 minutes): a full report is sent back-to-back, and
// a burst below it would refuse the same sorted tail at every resync.
const (
	workloadEventRatePerSec = 10.0
	workloadEventBurst      = workloadEventRatePerSec * 600
	workloadEventMaxRunners = 4096 // enrolLimiterMaxPeers' bound
)

// runnerEventRefusals is the per-runner bound on workload events, in memory. The bound on runner.event.refuse rows is
// the store's (runner_event_refusal_windows). The zero value is ready to use.
type runnerEventRefusals struct {
	mu        sync.Mutex
	workloads *principalLimiter // per-runner workload event rate, built on first use
}

// allowWorkload spends one of runnerID's workload events at now.
func (r *runnerEventRefusals) allowWorkload(runnerID uuid.UUID, now time.Time) bool {
	r.mu.Lock()
	if r.workloads == nil {
		r.workloads = &principalLimiter{rate: workloadEventRatePerSec, burst: workloadEventBurst, max: workloadEventMaxRunners}
	}
	l := r.workloads
	r.mu.Unlock()
	return l.allow(runnerID.String(), now)
}

// runnerEventRefuser is the store side of a refusal (store.PG): settled says it settled an action, row that it wrote
// the refusal's own row (false once the runner's window is full).
type runnerEventRefuser interface {
	RefuseRunnerEvent(ctx context.Context, runnerID uuid.UUID, r store.RunnerEventRefusal, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (settled, row bool, err error)
}

// runnerEventRefusalCodes are the permanent refusals, in the order they are matched; every other error is transient.
var runnerEventRefusalCodes = []struct {
	sentinel error
	code     string
}{
	{errRunnerEventShape, runnerwire.RefusalShape},
	{errRunnerEventUnsupported, runnerwire.RefusalUnsupportedKind},
	{errRunnerUnknownRun, runnerwire.RefusalUnknownRun},
	{store.ErrRunnerExecutionRefused, runnerwire.RefusalStoreRefused},
	{store.ErrRunnerActionRefused, runnerwire.RefusalStoreRefused},
	{store.ErrRunnerSpoolRefused, runnerwire.RefusalStoreRefused},
	{errRunnerEventRefused, runnerwire.RefusalStoreRefused},
	{errWorkloadEventRateLimited, runnerwire.RefusalRateLimited},
}

// classifyRunnerEvent turns err, an event handler's error, into the peer's verdict. A permanent refusal (a sentinel
// that names the event itself, never a bare not-found or a database error) is recorded first, a runner.event.refuse
// row within the store's per-runner bound and an action_result's open action settled as refused, and only once that commits is it an
// EventRefusal, which the peer acknowledges. Anything else is returned unchanged, so the link fails and the event is
// replayed; so is a refusal whose record failed.
func (s *Server) classifyRunnerEvent(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event, err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	code := ""
	for _, c := range runnerEventRefusalCodes {
		if errors.Is(err, c.sentinel) {
			sentinel, code = c.sentinel, c.code
			break
		}
	}
	if sentinel == nil {
		return err
	}
	st, ok := s.cfg.Store.(runnerEventRefuser)
	if !ok || s.cfg.RunnerAuditPrepare == nil {
		return fmt.Errorf("%w: %w", errRunnerIngestUnavailable, err)
	}
	sum := sha256.Sum256(ev.Data)
	r := store.RunnerEventRefusal{Kind: ev.Kind, Code: code, Sentinel: sentinel.Error(), RunID: ev.RunID,
		PayloadSHA256: hex.EncodeToString(sum[:]), PayloadBytes: len(ev.Data)}
	if ev.Kind == runnerwire.EventActionResult {
		// Lenient: a result refused for its shape still names the action it answers, if it names one at all.
		var named struct {
			ID uuid.UUID `json:"id"`
		}
		if json.Unmarshal(ev.Data, &named) == nil && named.ID != uuid.Nil {
			r.ActionID = &named.ID
		}
	}
	if ev.Kind == runnerwire.EventSpool && ev.RunID != nil {
		// Lenient too: a record refused for its shape or its run still says where it sits in its run's stream, and
		// the store moves the run's cursor past it only when it is next there.
		var named struct {
			SpoolID uuid.UUID `json:"spool_id"`
			Seq     int64     `json:"seq"`
			Hash    string    `json:"hash"`
		}
		if json.Unmarshal(ev.Data, &named) == nil && named.SpoolID != uuid.Nil {
			r.Spool = &store.RunnerEventRefusalSpool{RunID: *ev.RunID, SpoolID: named.SpoolID, Seq: named.Seq, Hash: named.Hash}
			// Read apart, so these fields can only matter to an overflow summary, never cost a record its place.
			var summary struct {
				Count   uint64 `json:"count"`
				DropEnd uint64 `json:"drop_end"`
			}
			if named.Seq == 0 && json.Unmarshal(ev.Data, &summary) == nil {
				r.Spool.Count, r.Spool.DropEnd = summary.Count, summary.DropEnd
			}
		}
	}
	settled, row, werr := st.RefuseRunnerEvent(ctx, runnerID, r, s.cfg.RunnerAuditPrepare)
	if werr != nil {
		slog.WarnContext(ctx, "runner event refusal not recorded; the event stays unacknowledged", "runner_id", runnerID,
			"kind", ev.Kind, "code", code, "error", werr)
		return err
	}
	if row || code != runnerwire.RefusalRateLimited {
		slog.WarnContext(ctx, "runner event refused for good and acknowledged", "runner_id", runnerID, "kind", ev.Kind,
			"run_id", ev.RunID, "code", code, "settled", settled)
	}
	return &runnerwire.EventRefusal{Code: code, Err: err}
}
