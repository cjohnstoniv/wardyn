// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pgRevocations is the pg-backed embedded.RevocationStore: jti-level OR
// run-level revocation over the identity_revocations table. Verify consults it
// with both the token jti and its run id, so a RevokeRun (run-level mark) is a
// true kill-switch cascade without enumerating every minted jti.
//
// Fail closed: any read error is treated as revoked by the caller (the embedded
// provider treats an IsRevoked error as a revoked token); here we surface the
// error so that contract holds.
type pgRevocations struct {
	pool *pgxpool.Pool
}

// runMarker is the deterministic per-run sentinel jti for run-level revocation.
// identity_revocations.jti is the PRIMARY KEY, so a run-level mark needs a
// unique-per-run key rather than a shared empty string.
func runMarker(runID uuid.UUID) string { return "run:" + runID.String() }

// IsRevoked reports revoked when the exact jti was revoked OR the run was
// revoked (the run-marker row exists). This is what makes RevokeRun a cascade.
func (r *pgRevocations) IsRevoked(ctx context.Context, jti string, runID uuid.UUID) (bool, error) {
	const q = `
		SELECT EXISTS(
			SELECT 1 FROM identity_revocations
			WHERE jti = $1 OR jti = $2
		)`
	var revoked bool
	if err := r.pool.QueryRow(ctx, q, jti, runMarker(runID)).Scan(&revoked); err != nil {
		return false, fmt.Errorf("wardynd: is-revoked query: %w", err)
	}
	return revoked, nil
}

// RevokeRun marks the whole run revoked via the per-run marker row. Verify then
// denies every current and future token bearing the run id. Idempotent.
func (r *pgRevocations) RevokeRun(ctx context.Context, runID uuid.UUID) error {
	const ins = `
		INSERT INTO identity_revocations (jti, run_id)
		VALUES ($1, $2)
		ON CONFLICT (jti) DO NOTHING`
	if _, err := r.pool.Exec(ctx, ins, runMarker(runID), runID); err != nil {
		return fmt.Errorf("wardynd: revoke run: %w", err)
	}
	return nil
}

// RevokeJTI revokes a single token by jti. Idempotent.
func (r *pgRevocations) RevokeJTI(ctx context.Context, jti string, runID uuid.UUID) error {
	const q = `
		INSERT INTO identity_revocations (jti, run_id)
		VALUES ($1, $2)
		ON CONFLICT (jti) DO NOTHING`
	if _, err := r.pool.Exec(ctx, q, jti, runID); err != nil {
		return fmt.Errorf("wardynd: revoke jti: %w", err)
	}
	return nil
}

// approvalStore adapts the function-style internal/store API + audit Recorder to
// the narrow approval.Store interface (one value implementing all five methods).
//
// FIX #5: rec is an audit.Recorder (the masked + SIEM-fanout recorder, maskedRec),
// NOT a plain store.Recorder. The approval FSM used to record decide/expire events
// straight to Postgres via store.Recorder, bypassing masking and the file/webhook/
// syslog sinks. Holding the interface here lets main.go inject maskedRec so those
// events fan out to SIEM exactly like idp/broker events.
type approvalStore struct {
	pool *pgxpool.Pool
	rec  audit.Recorder
}

func (a approvalStore) CreateApproval(ctx context.Context, ar types.ApprovalRequest) (types.ApprovalRequest, error) {
	return store.NewPG(a.pool).CreateApproval(ctx, ar)
}
func (a approvalStore) GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return store.NewPG(a.pool).GetApproval(ctx, id)
}
func (a approvalStore) ListApprovals(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	return store.NewPG(a.pool).ListApprovals(ctx, state)
}
func (a approvalStore) DecideApproval(ctx context.Context, id uuid.UUID, state types.ApprovalState, decidedBy, reason string) (types.ApprovalRequest, error) {
	return store.NewPG(a.pool).DecideApproval(ctx, id, state, decidedBy, reason)
}
func (a approvalStore) Record(ctx context.Context, ev types.AuditEvent) error {
	return a.rec.Record(ctx, ev)
}

var _ approval.Store = approvalStore{}

// approvalService implements api.ApprovalService over the approval FSM package.
// FIX #5: rec is the masked+fanout audit.Recorder (maskedRec), so decide events
// recorded by the FSM reach SIEM sinks, not just Postgres.
type approvalService struct {
	pool *pgxpool.Pool
	rec  audit.Recorder
}

func (s *approvalService) st() approvalStore { return approvalStore{pool: s.pool, rec: s.rec} }

func (s *approvalService) Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	return approval.RequestApproval(ctx, s.st(), req)
}
func (s *approvalService) Decide(ctx context.Context, id uuid.UUID, approve bool, decidedByType types.ActorType, decidedBy, reason string) (types.ApprovalRequest, error) {
	return approval.Decide(ctx, s.st(), id, approve, decidedByType, decidedBy, reason)
}
func (s *approvalService) Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return store.NewPG(s.pool).GetApproval(ctx, id)
}
func (s *approvalService) List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	return store.NewPG(s.pool).ListApprovals(ctx, state)
}

// ─── audit fanout ─────────────────────────────────────────────────────────────

// buildAuditFanout parses the -audit-sinks JSON config into a Fanout and starts
// the background Run loop of any sink that needs one (the webhook sink batches).
// Returns (nil, nil) when no sinks are configured. The returned Fanout's
// lifetime is the process; child Run goroutines stop when ctx is cancelled.
func buildAuditFanout(ctx context.Context, cfgJSON string) (*sinks.Fanout, error) {
	if cfgJSON == "" {
		return nil, nil
	}
	children, err := sinks.ParseSinks([]byte(cfgJSON))
	if err != nil {
		return nil, fmt.Errorf("parse audit sinks: %w", err)
	}
	if len(children) == 0 {
		return nil, nil
	}
	// Start the background flush loop for any sink that exposes one (webhook).
	for _, c := range children {
		if runner, ok := c.(interface{ Run(context.Context) }); ok {
			go runner.Run(ctx)
		}
	}
	return sinks.NewFanout(children...), nil
}

// fanoutRecorder writes every event to the primary store recorder (source of
// truth, append-only) and ALSO emits it to the sink fanout. The store write is
// authoritative: a fanout failure is logged but never returned, so audit
// streaming never gates the durable record (invariant 6).
type fanoutRecorder struct {
	primary store.Recorder
	fanout  *sinks.Fanout
}

var _ audit.Recorder = fanoutRecorder{}

func (f fanoutRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	err := f.primary.Record(ctx, ev)
	if f.fanout != nil {
		// Best-effort: log a total fanout failure, but do not propagate it.
		if ferr := f.fanout.Emit(ctx, ev); ferr != nil {
			log.Printf("wardynd: audit fanout emit failed (event %s): %v", ev.Action, ferr)
		}
	}
	return err
}

// ─── masking recorder ─────────────────────────────────────────────────────────

// maskingRecorder wraps an audit.Recorder and masks verbatim secret values from
// the ev.Data and ev.Target fields before delegating to the inner recorder. A
// nil Registry or a missing RunID are safe no-ops (the event is forwarded as-is).
//
// HONEST RESIDUAL: masking catches verbatim byte-identical leakage only; base64
// or model-narrated representations of secrets are NOT masked here.
type maskingRecorder struct {
	inner audit.Recorder
	reg   *secretmask.Registry
}

var _ audit.Recorder = maskingRecorder{}

func (m maskingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if m.reg != nil && ev.RunID != nil {
		snap := m.reg.Snapshot(*ev.RunID)
		if len(snap) > 0 {
			masker := secretmask.NewMasker(snap)
			if len(ev.Data) > 0 {
				masked := masker.Mask([]byte(ev.Data))
				ev.Data = json.RawMessage(masked)
			}
			if ev.Target != "" {
				ev.Target = string(masker.Mask([]byte(ev.Target)))
			}
		}
	}
	return m.inner.Record(ctx, ev)
}

// ─── spooling recorder ────────────────────────────────────────────────────────

// spoolingRecorder wraps an audit.Recorder so that when the inner (durable) write
// FAILS, the event is logged loudly and appended to a local append-only spool
// instead of being silently lost (invariant 6, C1). It is placed BELOW
// maskingRecorder in the chain, so the event it spools is already masked — closing
// the H9 leak where the API server's recordAudit spooled the PRE-masking event
// into audit-spool.jsonl. And because EVERY audit writer (API, broker, identity,
// approvals, sweeper) shares this recorder, all of them inherit the durable
// fallback — previously only the API server's recordAudit spooled, so broker
// credential.mint / identity / approval writes were log-only-lost on a PG outage.
type spoolingRecorder struct {
	inner audit.Recorder
	spool *api.AuditSpool // may be nil (spool unavailable) → log-only fallback
}

var _ audit.Recorder = spoolingRecorder{}

func (r spoolingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	err := r.inner.Record(ctx, ev)
	if err == nil {
		return nil
	}
	log.Printf("wardynd: AUDIT WRITE FAILED action=%s actor=%s outcome=%s: %v", ev.Action, ev.Actor, ev.Outcome, err)
	if r.spool != nil {
		if ferr := r.spool.Append(ev); ferr != nil {
			log.Printf("wardynd: AUDIT FALLBACK SPOOL FAILED action=%s: %v (EVENT LOST)", ev.Action, ferr)
		}
	}
	return err
}

// ─── lifecycle adapters ───────────────────────────────────────────────────────

// lifecycleStore adapts the function-style store package to lifecycle.Store.
// ListRunningWithPolicy joins agent_runs to run_policies and surfaces the
// per-policy auto_stop_after_sec (0 when no policy is attached). The policy spec
// is stored as JSONB, so auto_stop_after_sec is extracted with a JSON path.
type lifecycleStore struct {
	pool *pgxpool.Pool
}

var _ lifecycle.Store = lifecycleStore{}

func (l lifecycleStore) ListRunningWithPolicy(ctx context.Context) ([]lifecycle.RunSummary, error) {
	const q = `
		SELECT r.id, r.updated_at,
		       COALESCE((p.spec->>'auto_stop_after_sec')::int, 0) AS auto_stop_after_sec
		FROM agent_runs r
		LEFT JOIN run_policies p ON p.id = r.policy_id
		WHERE r.state = $1`
	rows, err := l.pool.Query(ctx, q, string(types.RunRunning))
	if err != nil {
		return nil, fmt.Errorf("wardynd: list running with policy: %w", err)
	}
	defer rows.Close()

	var out []lifecycle.RunSummary
	for rows.Next() {
		var s lifecycle.RunSummary
		if err := rows.Scan(&s.ID, &s.UpdatedAt, &s.PolicyAutoStopAfterSec); err != nil {
			return nil, fmt.Errorf("wardynd: scan run summary: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wardynd: iterate run summaries: %w", err)
	}
	return out, nil
}

// lifecycleStopper adapts the runner + store to lifecycle.Stopper. StopRun wins
// the idle-guarded RUNNING->STOPPED transition FIRST (so a run touched after the
// reaper's snapshot, or already moved terminal, is left alone), then gracefully
// stops the sandbox and runs the revoke cascade, surfacing any teardown/revoke
// failure to the reaper. It is idempotent: a missing sandbox or already-stopped
// run is not an error (the runner's StopSandbox is itself idempotent).
type lifecycleStopper struct {
	pool     *pgxpool.Pool
	runner   runner.Runner
	identity runRevoker // nil-safe; deny-lists the run token on idle stop
	broker   runRevoker // nil-safe; revokes minted broker credentials on idle stop
}

// runRevoker is the minimal revocation surface the idle reaper needs so a run
// stopped on idle also runs the kill-switch cascade's revocation half — matching
// the documented promise that revocation fires on every run stop, not only an
// explicit kill. Both *embedded.Provider and *broker.Broker satisfy it.
type runRevoker interface {
	RevokeRun(context.Context, uuid.UUID) error
}

var _ lifecycle.Stopper = lifecycleStopper{}

func (l lifecycleStopper) StopRun(ctx context.Context, runID uuid.UUID, notAfter time.Time) (lifecycle.StopOutcome, error) {
	run, err := store.NewPG(l.pool).GetRun(ctx, runID)
	if err != nil {
		return lifecycle.StopOutcome{}, fmt.Errorf("wardynd: lifecycle get run: %w", err)
	}
	// IDLE-GUARDED terminal transition FIRST (findings #1 + N3): move RUNNING->
	// STOPPED ONLY, and ONLY when updated_at has not advanced past the reaper's
	// snapshot (notAfter). This MUST precede the destructive StopSandbox: an active
	// `wardyn attach` TouchRun (which bumps updated_at, state stays RUNNING) between
	// the scan and here means the run is NOT idle — the guarded CAS then no-ops
	// (applied=false) and we tear nothing down and revoke nothing, preserving the
	// keepalive. If a concurrent kill/complete already moved the run terminal, the
	// CAS also no-ops and we leave that path's teardown/revocation untouched.
	applied, uerr := store.NewPG(l.pool).UpdateRunStateIfIdle(ctx, runID, types.RunRunning, types.RunStopped, notAfter)
	if uerr != nil {
		return lifecycle.StopOutcome{}, fmt.Errorf("wardynd: lifecycle update state: %w", uerr)
	}
	if !applied {
		return lifecycle.StopOutcome{Applied: false}, nil
	}

	// We won the idle RUNNING->STOPPED transition. Now free the sandbox and run the
	// kill-switch revocation half (deny-list the run token + revoke minted broker
	// creds), matching the documented cascade-on-every-stop. Each step is
	// best-effort + nil-safe and does NOT block the stop (the state is already
	// STOPPED), but a failure MUST be surfaced to the reaper (finding N1) — a
	// silently-failed teardown leaves a routable sandbox, a silently-failed revoke
	// leaves the run token valid until its <=1h TTL, while the audit says success.
	errs := map[string]string{}
	if l.runner != nil && run.SandboxRef != "" {
		if serr := l.runner.StopSandbox(ctx, run.SandboxRef); serr != nil {
			log.Printf("wardynd: lifecycle idle-stop sandbox teardown FAILED (run %s): %v -- sandbox may still be routable", runID, serr)
			errs["teardown_error"] = serr.Error()
		}
	}
	if l.identity != nil {
		if rerr := l.identity.RevokeRun(ctx, runID); rerr != nil {
			log.Printf("wardynd: lifecycle idle-stop identity revoke FAILED (run %s): %v -- run token may still be usable", runID, rerr)
			errs["identity_error"] = rerr.Error()
		}
	}
	if l.broker != nil {
		if rerr := l.broker.RevokeRun(ctx, runID); rerr != nil {
			log.Printf("wardynd: lifecycle idle-stop broker revoke FAILED (run %s): %v -- minted broker credentials may still be usable", runID, rerr)
			errs["broker_error"] = rerr.Error()
		}
	}
	out := lifecycle.StopOutcome{Applied: true}
	if len(errs) > 0 {
		out.Errors = errs
	}
	return out, nil
}

// runApprovalSweeper periodically transitions PENDING approvals older than
// `after` to EXPIRED via approval.ExpireStale, until ctx is cancelled. It mirrors
// the lifecycle reaper's goroutine shape; the first sweep runs after one tick.
func runApprovalSweeper(ctx context.Context, st approvalStore, interval, after time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := approval.ExpireStale(ctx, st, after)
			if err != nil {
				log.Printf("wardynd: approval sweep error: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("wardynd: approval sweep expired %d stale PENDING approval(s)", n)
			}
		}
	}
}
