// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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

var _ oidc.RoleMappingSource = (*pgRoleMappings)(nil)

// roleMappingsFor returns the pg-backed console role-mapping source wired
// into oidc.Config.RoleMappings — unconditional, the same reasoning
// pgSessionRevocations gets wired directly (not through a nil-guarded
// helper) at its own oidc.Config call site: pool is already required
// whenever OIDC boots at all (Postgres is the one required dependency), so
// this is never nil once OIDC is configured. Split out as its own tiny
// function (rather than inlined at the oidc.Config literal, unlike
// Revocations) so it is unit-testable without a live OIDC discovery round
// trip — buildOptionalFeatures itself needs a real IdP to exercise.
func roleMappingsFor(pool *pgxpool.Pool) oidc.RoleMappingSource {
	return &pgRoleMappings{pool: pool}
}

// pgRoleMappings is the pg-backed oidc.RoleMappingSource (Phase 2 lane A): a
// thin read of role_mappings (migration 0051), consulted once per login by
// CallbackHandler/PreviewRole and merged with the chart's WARDYN_OIDC_ROLE_MAP
// (see mergeRoleMaps). Distinct from pgSessionRevocations above — a different
// table, a different concern (who gets which role vs. whose session is
// still valid) — but the SAME store -> oidc bridge shape: a stateless
// wrapper over the shared pool, constructed fresh per Config rather than
// shared, because it holds no state of its own.
type pgRoleMappings struct {
	pool *pgxpool.Pool
}

// ListRoleMappings delegates to the store — this adapter exists only so
// internal/auth/oidc never imports internal/store. The conversion from
// types.RoleMapping (id/timestamps/provenance) to oidc.RoleMapping (bare
// Value/Role/UserType) happens here, the one place both types are in scope.
func (r *pgRoleMappings) ListRoleMappings(ctx context.Context) ([]oidc.RoleMapping, error) {
	rows, err := store.NewPG(r.pool).ListRoleMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("wardynd: list role mappings: %w", err)
	}
	out := make([]oidc.RoleMapping, len(rows))
	for i, m := range rows {
		out[i] = oidc.RoleMapping{Value: m.Value, Role: m.Role, UserType: m.UserType}
	}
	return out, nil
}

// approvalStore satisfies the narrow approval.Store interface: the embedded
// store.PG carries the four CRUD methods verbatim, and only Record is adapted.
//
// FIX #5: rec is an audit.Recorder (the masked + SIEM-fanout recorder, maskedRec),
// NOT a plain store.Recorder. The approval FSM used to record decide/expire events
// straight to Postgres via store.Recorder, bypassing masking and the file/webhook/
// syslog sinks. Holding the interface here lets main.go inject maskedRec so those
// events fan out to SIEM exactly like idp/broker events. (store.PG has no Record
// method, so this one can never be shadowed back to the plain store recorder.)
type approvalStore struct {
	store.PG
	rec audit.Recorder
}

func (a approvalStore) Record(ctx context.Context, ev types.AuditEvent) error {
	return a.rec.Record(ctx, ev)
}

var _ approval.Store = approvalStore{}

// approvalService implements api.ApprovalService over the approval FSM package.
// FIX #5: st.rec is the masked+fanout audit.Recorder (maskedRec), so decide
// events recorded by the FSM reach SIEM sinks, not just Postgres.
type approvalService struct {
	st approvalStore
}

func (s *approvalService) Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	return approval.RequestApproval(ctx, s.st, req)
}
func (s *approvalService) Decide(ctx context.Context, id uuid.UUID, decidedByType types.ActorType, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	return approval.Decide(ctx, s.st, id, decidedByType, decision)
}
func (s *approvalService) Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return s.st.GetApproval(ctx, id)
}
func (s *approvalService) List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	return s.st.ListApprovals(ctx, state)
}
func (s *approvalService) CancelForRun(ctx context.Context, runID uuid.UUID, reason string) (map[string]int, error) {
	return approval.CancelForRun(ctx, s.st, runID, reason)
}
func (s *approvalService) ExpireOne(ctx context.Context, id uuid.UUID, actor, reason string) error {
	return approval.ExpireOne(ctx, s.st, id, actor, reason)
}
func (s *approvalService) CountForRun(ctx context.Context, runID uuid.UUID) (int, error) {
	return s.st.CountApprovalsForRun(ctx, runID)
}

// ListApprovalsPage is the OPTIONAL paged lister the api handler type-asserts
// for (same pattern as store.Pager): the console polls four single-state lists
// every 10s and decided approvals are never deleted, so the unpaged read grew
// with deployment age. Promoted from the embedded store.PG; pure delegation,
// like List — the approval FSM owns decisions, not reads.
func (s *approvalService) ListApprovalsPage(ctx context.Context, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error) {
	return s.st.ListApprovalsPage(ctx, state, p)
}

// ListApprovalsPageByRunCreator is item 2 / M2's ownership-scoped analogue of
// ListApprovalsPage above — the api handler type-asserts for it (same
// optional-capability pattern as store.Pager) to serve a member's unscoped
// GET /approvals fail-closed rather than fail-open. Pure delegation, promoted
// from the embedded store.PG exactly like ListApprovalsPage.
func (s *approvalService) ListApprovalsPageByRunCreator(ctx context.Context, createdBy string, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error) {
	return s.st.ListApprovalsPageByRunCreator(ctx, createdBy, state, p)
}

var _ store.ApprovalsByRunCreatorPager = (*approvalService)(nil)

// ListApprovalsPageByRun is the ?run_id= analogue — the shape the CLI and the
// console's run detail page poll. Without this delegation the handler's
// type-assert misses and a run-scoped list falls back to reading EVERY approval
// row in the deployment and filtering in Go (F072). Pure delegation, promoted
// from the embedded store.PG exactly like ListApprovalsPage.
func (s *approvalService) ListApprovalsPageByRun(ctx context.Context, runID uuid.UUID, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error) {
	return s.st.ListApprovalsPageByRun(ctx, runID, state, p)
}

var _ store.ApprovalsByRunPager = (*approvalService)(nil)

// ListPendingApprovalsForRuns is #1197's attention-projection read: the api
// handler type-asserts for it on s.cfg.Approvals (not s.cfg.Store — see that
// lane's own finding on where this capability belongs), the same
// optional-capability pattern as the two pagers above. Pure delegation,
// promoted from the embedded store.PG.
func (s *approvalService) ListPendingApprovalsForRuns(ctx context.Context, runIDs []uuid.UUID) ([]types.ApprovalRequest, error) {
	return s.st.ListPendingApprovalsForRuns(ctx, runIDs)
}

// CountPendingApprovals / CountPendingApprovalsByRunCreator back
// GET /me/attention's pending_approvals count in each view — counted in the
// database rather than listed, the same fix CountApprovalsForRun's own doc
// argues for. Pure delegation, promoted from the embedded store.PG.
func (s *approvalService) CountPendingApprovals(ctx context.Context) (int, error) {
	return s.st.CountPendingApprovals(ctx)
}

func (s *approvalService) CountPendingApprovalsByRunCreator(ctx context.Context, createdBy string) (int, error) {
	return s.st.CountPendingApprovalsByRunCreator(ctx, createdBy)
}

var _ store.ApprovalsForRunsPager = (*approvalService)(nil)

// ApprovalEscalations / ApprovalNotifyChannelStats back the approvals list's escalation chips and
// GET /approval-notify/status. Pure delegation, promoted from the embedded store.PG.
func (s *approvalService) ApprovalEscalations(ctx context.Context, ids []uuid.UUID, now time.Time) (map[uuid.UUID]types.ApprovalEscalation, error) {
	return s.st.ApprovalEscalations(ctx, ids, now)
}

func (s *approvalService) ApprovalNotifyChannelStats(ctx context.Context, now time.Time) ([]types.ApprovalNotifyChannelStat, error) {
	return s.st.ApprovalNotifyChannelStats(ctx, now)
}

var _ store.ApprovalNotifyReader = (*approvalService)(nil)

// Audit fanout

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
	//
	// WithoutCancel: SIGTERM cancels ctx, and a flusher that stops THERE returns
	// before httpSrv.Shutdown has finished — leaving up to 15 seconds of
	// in-flight handlers (plus FlushAuthFailedStreak's summary row) enqueueing
	// into a 4096-slot buffer with no reader, dropped without even a counter,
	// while fan.Close() found `done` already closed and returned instantly. The
	// lifetime that is correct here is the FANOUT's, not the request tree's:
	// Fanout.Close → WebhookSink.Close drains and is bounded by client.Timeout,
	// and serveAndShutdown calls it on every exit path.
	for _, c := range children {
		if runner, ok := c.(interface{ Run(context.Context) }); ok {
			runCtx := context.WithoutCancel(ctx)
			go goSafe("audit.sink.flush", func() { runner.Run(runCtx) })
		}
	}
	return sinks.NewFanout(children...), nil
}

// siemSink is fan as an audit.Sink, nil when no sink is configured (a nil *Fanout in the interface would
// panic on Emit).
func siemSink(fan *sinks.Fanout) audit.Sink {
	if fan == nil {
		return nil
	}
	return fan
}

// armKeyDestroySIEM has the secret store's subject keys send principal_key.destroyed to siem.
func armKeyDestroySIEM(secrets secretstore.Store, siem audit.Sink) {
	if m := subjectKeysOf(secrets); m != nil && siem != nil {
		m.WithSIEM(siem)
	}
}

// sinkDropsReporter adapts a Fanout to the api.Config.AuditSinkDrops callback
// (D2). Returns nil when no fanout is configured so the metric is omitted rather
// than reporting an empty map on every scrape.
func sinkDropsReporter(fan *sinks.Fanout) func() map[string]int64 {
	if fan == nil {
		return nil
	}
	return fan.DropsByName
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
	// store.InsertAuditEvent is called directly rather than through
	// f.primary.Record for ONE reason: it takes ev by POINTER and fills in the
	// hash chain Postgres computed (migration 0047), so what fans out to the
	// sinks below carries prev_hash and this row's own row_hash — the CURRENT
	// CHAIN HEAD at the moment of the write. That is what lets an external SIEM
	// detect a later truncation: it holds head hashes off-box, and a chain that
	// no longer contains one it saw has been rewritten. audit.Recorder takes ev
	// by value, so Recorder.Record structurally cannot hand them back.
	// A failed store write leaves both empty and the event still fans out.
	err := store.InsertAuditEvent(ctx, f.primary.Pool, &ev)
	if f.fanout != nil {
		// Best-effort: log a total fanout failure, but do not propagate it.
		if ferr := f.fanout.Emit(ctx, ev); ferr != nil {
			slog.ErrorContext(ctx, "wardynd: audit fanout emit failed",
				slog.String("action", ev.Action),
				slog.Any("err", ferr),
			)
		}
	}
	return err
}

// landedRecorder writes an event to the store and emits it to the sink fanout only once the store took it,
// with the chain hashes the database computed. The spool drain uses it for a row that never reached the
// sinks while it waited under the pending key; a failed write emits nothing, so the drain's retry cannot
// send it twice.
type landedRecorder struct {
	primary store.Recorder
	fanout  *sinks.Fanout
}

var _ audit.Recorder = landedRecorder{}

func (l landedRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := store.InsertAuditEvent(ctx, l.primary.Pool, &ev); err != nil {
		return err
	}
	if ferr := l.fanout.Emit(context.WithoutCancel(ctx), ev); ferr != nil {
		slog.ErrorContext(ctx, "wardynd: audit fanout emit failed",
			slog.String("action", ev.Action),
			slog.Any("err", ferr),
		)
	}
	return nil
}

// Masking recorder

// maskingRecorder wraps an audit.Recorder and masks verbatim secret values from
// the ev.Data and ev.Target fields before delegating to the inner recorder. A
// nil Registry is a safe no-op (the event is forwarded as-is). A missing RunID
// still masks against the PROCESS-GLOBAL corpus (see Record) — there is simply
// no PER-RUN corpus to add on top of it.
//
// HONEST RESIDUAL: masking catches verbatim byte-identical leakage only; base64
// or model-narrated representations of secrets are NOT masked here.
type maskingRecorder struct {
	inner audit.Recorder
	reg   *secretmask.Registry
	// scope labels a run's rows "mask_scope":"globals_only" while this process
	// does not hold the run's complete masking manifest. Nil labels nothing.
	scope *maskScope
}

var _ audit.Recorder = maskingRecorder{}

func (m maskingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	// Capped at the top of the chain as well as at the INSERT (B6-F1): this
	// recorder is outermost, so capping here is what bounds the SPOOL and the
	// SIEM SINKS too — store.InsertAuditEvent's own cap only protects the
	// database. The target is `r.URL.Path` on the authz.denied lane, which the
	// AUTHENTICATED caller drives with no rate limit at all.
	//
	// Before the masker, not after: masking a megabyte of attacker-chosen path
	// is work nobody asked for, and the mask is per-byte either way.
	ev.Target = store.CapAuditTarget(ev.Target)
	if ev.RunID != nil && m.scope.uncovered(*ev.RunID) {
		ev.Data = withMaskScope(ev.Data)
	}
	if m.reg != nil {
		// A run-less event (ev.RunID == nil —
		// policy.inline.apply, secret.*, an admin action) must still fall back to the
		// PROCESS-GLOBAL corpus (Bedrock SSO / subscription creds registered
		// via AddGlobal) rather than bypass masking entirely — the guard here
		// is `m.reg != nil` alone, never also `ev.RunID != nil`. The uuid.Nil
		// corpus is exactly that — globals only, since uuid.Nil is never a
		// real run's perRun key — so a run-less row is masked against the
		// same globals every real run already is, just with no per-run
		// corpus layered on top (there is none to add: masking is per-run,
		// and without a run id there is no run-scoped snapshot to apply — a
		// registered secret for some OTHER
		// run must still never leak into a run-less event's masking).
		runID := uuid.Nil
		if ev.RunID != nil {
			runID = *ev.RunID
		}
		// D31: ev.Data is JSON, so a registered secret bearing a newline, quote,
		// or backslash (e.g. a minted ssh_key PEM) lands there in its JSON-escaped
		// form, which the raw-value masker would miss — hence the escaped-variant
		// expansion, the same one the recording-upload path applies to asciicast
		// bodies.
		//
		// JSONVariantMasker, not NewMasker(JSONEscapedVariants(Snapshot(...))):
		// that chain clones the corpus, triples it through a documented O(n^2)
		// de-dup, and sorts the result — on EVERY audit event (F076). The registry
		// now derives it once per generation and hands back the same immutable
		// masker until something is registered or evicted.
		masker := m.reg.JSONVariantMasker(runID)
		if len(masker.Secrets()) > 0 {
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

// Spooling recorder

// spoolingRecorder wraps an audit.Recorder so that when the inner (durable) write
// FAILS, the event is logged loudly and appended to a local append-only spool
// instead of being silently lost (invariant 6, C1). It is placed BELOW
// maskingRecorder in the chain, so the event it spools is already masked —
// never the PRE-masking event, which must not land in audit-spool.jsonl. And
// because EVERY audit writer (API, broker, identity, approvals, sweeper)
// shares this recorder, all of them inherit the durable fallback: none of
// broker credential.mint / identity / approval writes are log-only-lost on a
// PG outage.
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
	slog.ErrorContext(ctx, "wardynd: AUDIT WRITE FAILED",
		slog.String("action", ev.Action),
		slog.String("actor", ev.Actor),
		slog.String("outcome", ev.Outcome),
		slog.Any("err", err),
	)
	if r.spool != nil {
		if ferr := r.spool.Append(ev); ferr != nil {
			slog.ErrorContext(ctx, "wardynd: AUDIT FALLBACK SPOOL FAILED (EVENT LOST)",
				slog.String("action", ev.Action),
				slog.Any("err", ferr),
			)
		}
	}
	return err
}

// Lifecycle adapters

// lifecycleStore adapts the function-style store package to lifecycle.Store.
// ListRunningWithPolicy reads each run's EFFECTIVE idle cap from the run row's
// auto_stop_after_sec column (captured from the resolved policy at CreateRun),
// NOT by LEFT JOINing run_policies on policy_id and reading the policy JSONB:
// inline/default/scan/verify/record/harness-login runs have no stored
// policy_id, so that join comes back NULL and COALESCEs to 0 (never
// auto-stop), silently exempting every such run from idle reaping.
type lifecycleStore struct {
	pool *pgxpool.Pool
}

var _ lifecycle.Store = lifecycleStore{}

func (l lifecycleStore) ListRunningWithPolicy(ctx context.Context) ([]lifecycle.RunSummary, time.Time, error) {
	// now() comes back with the rows, and it is the same instant on every one of
	// them: now() is the transaction's start time, so a single statement reads
	// one clock for the whole scan. updated_at is stamped by that same clock, so
	// the reaper's subtraction is finally two readings of ONE clock — wardynd's
	// own was the skew that stopped actively-attached runs (B8-F2).
	//
	// A KEPT run (lost_at set: its lease ended it) is not idle, it is stopped;
	// the ended-run grace decides when its files go, not auto_stop_after_sec.
	// The one exception is a run kept after an outage whose agent still runs
	// (store.HoldsSandboxSQL): it is listed as Kept, for the max age alone.
	//
	// An EMPTY scan returns the zero time, which the reaper reads as "no clock":
	// there are no rows to measure, so there is nothing for it to be wrong about,
	// and a second round trip to fetch a clock nobody would use is not worth it.
	const q = `
		SELECT id, created_at, updated_at, auto_stop_after_sec, lost_at IS NOT NULL, now()
		FROM agent_runs
		WHERE state = $1 AND ` + store.HoldsSandboxSQL
	rows, err := l.pool.Query(ctx, q, string(types.RunRunning))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("wardynd: list running with policy: %w", err)
	}
	defer rows.Close()

	var out []lifecycle.RunSummary
	var dbNow time.Time
	for rows.Next() {
		var s lifecycle.RunSummary
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt, &s.PolicyAutoStopAfterSec, &s.Kept, &dbNow); err != nil {
			return nil, time.Time{}, fmt.Errorf("wardynd: scan run summary: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("wardynd: iterate run summaries: %w", err)
	}
	return out, dbNow, nil
}

// reapTickLock is the reaper's single-flight gate: a Postgres try-advisory-lock
// so two control planes (or an old process still winding down beside a new one)
// cannot both scan and stop the same runs on the same tick. Follows
// lifecycleStore/lifecycleStopper — the pool stays here in wardynd and lifecycle
// keeps its target-agnostic seams. A lock we cannot reach skips the tick: the
// scan that follows would fail on the same database anyway.
//
// It also prunes stale AWS SSO spent-token rows (#149) once the lock is won,
// rather than starting a THIRD timer beside this one and the two other
// sweepers: the lock is already single-flight across every control plane, and
// a prune racing the idle-run scan on the shared pool is harmless (different
// table, no shared lock). Best-effort — a prune failure never fails the tick
// or blocks reaping.
func reapTickLock(pool *pgxpool.Pool) func(context.Context) (func(), bool) {
	return func(ctx context.Context) (func(), bool) {
		release, ok, err := db.TryAdvisoryLock(ctx, pool, db.ReaperAdvisoryLockKey)
		if err != nil {
			slog.DebugContext(ctx, "wardynd: reap tick lock unavailable", slog.Any("err", err))
			return nil, false
		}
		if ok {
			pruneAWSSSOSpentTokens(ctx, pool)
		}
		return release, ok
	}
}

// pruneAWSSSOSpentTokens deletes AWS SSO spent-token rows older than
// api.AWSSSOSpentTokenRetention. Called from reapTickLock, under the reap
// advisory lock. Best-effort: a failure is logged and the tick proceeds — the
// rows are a bounded cache-fill cost, not a correctness requirement, so a
// prune miss on one tick is retried on the next.
func pruneAWSSSOSpentTokens(ctx context.Context, pool *pgxpool.Pool) {
	n, err := store.NewPG(pool).PruneAWSSSOSpentTokens(ctx, time.Now().Add(-api.AWSSSOSpentTokenRetention))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: pruning aws sso spent tokens failed", slog.Any("err", err))
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: pruned stale aws sso spent-token rows", slog.Int("deleted", n))
	}
}

// terminalSandboxSweepTickLock is the terminal-sandbox sweep ticker's
// single-flight gate: the same shape as reapTickLock, a different key
// (db.TerminalSandboxSweepLockKey) — see that key's own doc for why
// claimSingleInstance alone is not enough. A lock we cannot reach skips the
// tick entirely; the sweep that follows would fail on the same database
// anyway.
func terminalSandboxSweepTickLock(pool *pgxpool.Pool) func(context.Context) (func(), bool) {
	return func(ctx context.Context) (func(), bool) {
		release, ok, err := db.TryAdvisoryLock(ctx, pool, db.TerminalSandboxSweepLockKey)
		if err != nil {
			slog.DebugContext(ctx, "wardynd: terminal sandbox sweep tick lock unavailable", slog.Any("err", err))
			return nil, false
		}
		return release, ok
	}
}

// groundtruthRotatorLock is the ground-truth rotator's leader-election gate
// (S2): a Postgres try-advisory-lock acquired ONCE (not per-tick, unlike
// reapTickLock above) so at most one replica runs the mint/write loop in the
// steady state while every other replica parks on a backoff. Held for the
// process lifetime, so on a connection of its own rather than a pooled one —
// and only probed from the pool first (db.AdvisoryLockHeld), so a standby
// retrying every groundtruthRotatorLockBackoff does not dial a session it is
// about to close again. NOT a fencing primitive — a lost session can transiently
// leave two leaders; see runGroundtruthTokenRotatorLeader (gt_rotator.go).
// Unlike reapTickLock this surfaces err separately from a plain not-acquired so
// the caller can log standby state (lock genuinely held elsewhere) distinctly
// from an unreachable database; a probe that fails falls through to the
// dedicated try, which reports the real fault.
func groundtruthRotatorLock(pool *pgxpool.Pool) func(context.Context) (func(), bool, error) {
	return func(ctx context.Context) (func(), bool, error) {
		if held, err := db.AdvisoryLockHeld(ctx, pool, db.GroundTruthRotatorLockKey); err == nil && held {
			return nil, false, nil // held elsewhere, and no session was dialled to find out
		}
		_, release, ok, err := db.TryAdvisoryLockDedicated(ctx, pool, db.GroundTruthRotatorLockKey)
		return release, ok, err
	}
}

// lifecycleStopper adapts the runner + store to lifecycle.Stopper. StopRun wins
// the idle-guarded RUNNING->STOPPED transition FIRST (so a run touched after the
// reaper's snapshot, already moved terminal, or with an open request still inside
// its wait (store.openHoldSQL) is left alone), then gracefully
// stops the sandbox and runs the revoke cascade, surfacing any teardown/revoke
// failure to the reaper. It is idempotent: a missing sandbox or already-stopped
// run is not an error (the runner's StopSandbox is itself idempotent).
type lifecycleStopper struct {
	pool     *pgxpool.Pool
	runner   runner.Runner
	identity runRevoker // nil-safe; deny-lists the run token on idle stop
	broker   runRevoker // nil-safe; revokes minted broker credentials on idle stop
	// cancelApprovals is the APPROVAL half of the same cascade — api.Server's
	// CancelTerminalRunApprovals, threaded in rather than reimplemented, because
	// this is the THIRD terminal writer and the other two already call it. Nil-safe
	// (an embedding with no server, and every test double, leaves it unset).
	//
	// Why it is a func and not an approval store: the reason stamped on each
	// cancelled row is read back from the run row by the server's own
	// terminalCancelReason, so the reaper hands over a run id and nothing it
	// could get wrong; and the store this reaper holds is a pool, not the
	// approval service the API server already owns.
	cancelApprovals func(context.Context, uuid.UUID)
	// finishOutput is the run-output finalisation contract (api.Server's
	// FinishRunOutput), reached through the server rather than copied here.
	// Nil-safe, like cancelApprovals.
	finishOutput func(context.Context, uuid.UUID)
	// snapshotPane is api.Server's SnapshotRunPane: an interactive run's pane
	// snapshot, taken after the revocations and before StopSandbox. Nil-safe.
	snapshotPane func(context.Context, uuid.UUID)
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
	return l.stop(ctx, runID, func() (bool, error) {
		return store.NewPG(l.pool).UpdateRunStateIfIdle(ctx, runID, types.RunRunning, types.RunStopped, notAfter)
	})
}

// StopRunMaxAge is StopRun for a run past WARDYN_RUN_MAX_AGE: the same teardown
// and revocation, behind a transition guarded on the run's age alone.
func (l lifecycleStopper) StopRunMaxAge(ctx context.Context, runID uuid.UUID, createdNotAfter time.Time) (lifecycle.StopOutcome, error) {
	return l.stop(ctx, runID, func() (bool, error) {
		return store.NewPG(l.pool).UpdateRunStateIfCreatedBefore(ctx, runID, types.RunRunning, types.RunStopped, createdNotAfter)
	})
}

// stop wins transition (a guarded RUNNING->STOPPED), then tears down and revokes.
func (l lifecycleStopper) stop(ctx context.Context, runID uuid.UUID, transition func() (bool, error)) (lifecycle.StopOutcome, error) {
	run, err := store.NewPG(l.pool).GetRun(ctx, runID)
	if err != nil {
		return lifecycle.StopOutcome{}, fmt.Errorf("wardynd: lifecycle get run: %w", err)
	}
	// IDLE-GUARDED terminal transition FIRST (findings #1 + N3): move RUNNING->
	// STOPPED ONLY, and ONLY when updated_at has not advanced past the reaper's
	// snapshot (notAfter). This MUST precede the destructive StopSandbox: an active
	// `wardyn run attach` TouchRun (which bumps updated_at, state stays RUNNING) between
	// the scan and here means the run is NOT idle — the guarded CAS then no-ops
	// (applied=false) and we tear nothing down and revoke nothing, preserving the
	// keepalive. If a concurrent kill/complete already moved the run terminal, or
	// an open request is still inside its wait (store.openHoldSQL), the CAS also
	// no-ops and we leave the run and any teardown/revocation untouched.
	applied, uerr := transition()
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
	// APPROVALS FIRST, before the destructive teardown, for the same reason
	// handleKillRun cancels before KillSandbox: a PENDING approval is the one
	// piece of this cascade a human is looking at, and an idle-stopped run is
	// often idle BECAUSE its agent is parked on a hold — once that hold's wait
	// has passed, since the CAS refuses while an open request is still inside
	// its wait (store.openHoldSQL). Run
	// AFTER the guarded CAS for the same reason the revokes are: a stop that lost
	// the CAS must not cancel a still-live run's questions. Best-effort and
	// non-blocking like the revokes (the server logs + audits its own failure);
	// the ExpireStale sweeper stays behind it as the backstop it always was.
	if l.cancelApprovals != nil {
		l.cancelApprovals(ctx, runID)
	}
	errs := map[string]string{}
	// Revocations BEFORE the teardown, so the pane snapshot's seconds never
	// extend a live credential: finalizeRunTail's order (revoke, snapshot,
	// StopSandbox, finish). The CAS above still decides first whether any of it runs.
	if l.identity != nil {
		if rerr := l.identity.RevokeRun(ctx, runID); rerr != nil {
			slog.ErrorContext(ctx, "wardynd: lifecycle idle-stop identity revoke FAILED -- run token may still be usable",
				slog.String("run_id", runID.String()),
				slog.Any("err", rerr),
			)
			errs["identity_error"] = rerr.Error()
		}
	}
	if l.broker != nil {
		if rerr := l.broker.RevokeRun(ctx, runID); rerr != nil {
			slog.ErrorContext(ctx, "wardynd: lifecycle idle-stop broker revoke FAILED -- minted broker credentials may still be usable",
				slog.String("run_id", runID.String()),
				slog.Any("err", rerr),
			)
			errs["broker_error"] = rerr.Error()
		}
	}
	if l.snapshotPane != nil {
		l.snapshotPane(ctx, runID)
	}
	if l.runner != nil && run.SandboxRef != "" {
		if serr := l.runner.StopSandbox(ctx, run.SandboxRef); serr != nil {
			slog.ErrorContext(ctx, "wardynd: lifecycle idle-stop sandbox teardown FAILED -- sandbox may still be routable",
				slog.String("run_id", runID.String()),
				slog.Any("err", serr),
			)
			errs["teardown_error"] = serr.Error()
		}
	}
	if l.finishOutput != nil {
		l.finishOutput(ctx, runID)
	}
	out := lifecycle.StopOutcome{Applied: true}
	if len(errs) > 0 {
		out.Errors = errs
	}
	return out, nil
}

// attachLoginGrantSink joins the console login to the credential capture, and
// owns the nil check so run() does not: authn is nil on every deployment
// without SSO, and there is no login to widen there.
//
// The edge is attached rather than configured because the two sides form a
// cycle — oidc.Config is built before the server, and the server holds the
// Authenticator — so the only order that works is "construct both, then join".
func attachLoginGrantSink(authn *oidc.Authenticator, sink oidc.LoginGrantSink) {
	if authn == nil {
		return
	}
	authn.AttachLoginGrantSink(sink)
}
