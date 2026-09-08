// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"database/sql"
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
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var _ oidc.SessionRevocations = (*pgSessionRevocations)(nil)

// sessionRevocationsFor returns the D16 pg-backed revocations store when OIDC
// is actually configured (authn != nil), else nil — mirrors "OIDC:
// feats.authn" on api.Config being nil exactly when OIDC is unconfigured, so
// the admin revoke-sessions surface never mounts with no session mechanism
// for it to act on. A second, independent *pgSessionRevocations instance from
// the one buildOptionalFeatures wires into oidc.Config.Revocations — both are
// stateless wrappers over the same pool, so two instances cost nothing.
func sessionRevocationsFor(authn *oidc.Authenticator, pool *pgxpool.Pool) oidc.SessionRevocations {
	if authn == nil {
		return nil
	}
	return &pgSessionRevocations{pool: pool}
}

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

// globalRevokeSub is the reserved oidc_session_revocations.sub sentinel for a
// revoke-all — see the migration's doc comment.
const globalRevokeSub = ""

// pgSessionRevocations is the pg-backed oidc.SessionRevocations (D16): a
// per-principal (and global) revoke CUTOFF over oidc_session_revocations,
// checked by internal/auth/oidc's Middleware on every authenticated request
// once wired. Distinct from pgRevocations above, which is the per-run SPIFFE
// identity denylist — a different table, a different session concept
// entirely (a stateless signed cookie has no row of its own to delete).
type pgSessionRevocations struct {
	pool *pgxpool.Pool
	// now is the APP clock IsSessionRevoked measures a credential's age on; nil
	// means time.Now. A test injects a clock that runs ahead of the database's,
	// which is the only honest way to simulate the F289 skew: the age helper
	// clamps a stamp from its own future to zero, so handing IsSessionRevoked an
	// issuedAt ahead of the real clock does not model a fast wardynd — it models
	// a stamp the app itself could never have written.
	now func() time.Time
}

func (r *pgSessionRevocations) appNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// IsSessionRevoked reports revoked when issuedAt is at-or-before the LATER of
// the cutoffs matching this human and the global one — a single query (MAX
// over the candidate rows) so a caller with no wired revocations at all (the
// common case: no row for either identity or globally) pays one lookup and
// gets back SQL NULL, which is "never revoked", not a zero-time false alarm.
//
// THREE candidate keys, because a revoke may name either identity (see
// oidc.SessionRevocations): the sub EXACTLY — an OIDC sub is opaque and
// case-sensitive, so folding it could collide two distinct principals — the
// email CASE-INSENSITIVELY, since that is how a human types one and the admin
// naming a target has no reason to match the IdP's casing, and the reserved ""
// global row.
//
// An empty email needs NO guard, and adding one would be unpinnable defensive
// code: lower(sub) = lower(”) selects exactly the sub = ” row, which is the
// global row the third arm already selects. The two arms return the same
// cutoff, so a session with no email claim behaves identically either way —
// verified by removing a NULLIF guard and finding no test could tell the
// difference, because there is no difference to tell.
//
// lower(sub) defeats the index on this arm. Deliberate: oidc_session_revocations
// holds one row per revoked principal plus the global one — tens of rows on a
// real deployment, not a scan worth an expression index — and the alternative
// (folding at write time) cannot work, since the writer does not know whether
// the caller named a sub or an email.
func (r *pgSessionRevocations) IsSessionRevoked(ctx context.Context, sub, email string, issuedAt time.Time) (bool, error) {
	// ASKED ON BOTH CLOCKS, AND EITHER ANSWER OF "REVOKED" WINS.
	//
	// revoked_at is stamped by POSTGRES. issuedAt is stamped by WARDYND — and by
	// wardynd in two different senses, which is why this cannot simply pick one
	// clock and convert: an SSO cookie's `iat` is a wall-clock reading taken when
	// the cookie was minted, while an API token's created_at is now written on
	// the database's own clock (store.CreateAPIToken). This function is handed
	// both and cannot tell them apart, and there is no signature here to widen —
	// the interface is internal/auth/oidc's.
	//
	// So it asks the question twice and takes the earlier-revoking answer:
	// directly against the cutoff (exact when issuedAt is already on the database
	// clock), and against the database's now() minus the age wardynd measured for
	// it (exact when issuedAt is an app wall-clock reading). Under a skew of d
	// the two disagree by at most d, and OR-ing them means the disagreement
	// always resolves toward REVOKED. That asymmetry is the whole point: a revoke
	// that fires d early during a clock skew is a session re-authenticating; a
	// revoke that fires d late is the admin's "revoke every session for this
	// human" silently not doing it, which is the finding.
	//
	// The age is measured entirely on wardynd's clock (now minus issuedAt), so no
	// skew rides in on it — see db.AppClockAgeMicros, whose contract is that both
	// of its arguments come from one clock.
	q := `
		SELECT MAX(revoked_at), MAX(revoked_at) >= ` + db.AppClockAgeSQL("$4") + `
		FROM oidc_session_revocations
		WHERE sub = $1
		   OR lower(sub) = lower($2)
		   OR sub = $3`
	var cutoff sql.NullTime
	var byDBClock sql.NullBool
	age := db.AppClockAgeMicros(issuedAt, r.appNow())
	if err := r.pool.QueryRow(ctx, q, sub, email, globalRevokeSub, age).Scan(&cutoff, &byDBClock); err != nil {
		return false, fmt.Errorf("wardynd: is-session-revoked query: %w", err)
	}
	if !cutoff.Valid {
		return false, nil // no revocation on record for this sub or globally
	}
	// issuedAt.IsZero() (a pre-D16 cookie with no iat) sorts before EVERY real
	// cutoff, so it reads as revoked the moment any matching row exists at
	// all — see oidc.SessionRevocations' doc comment for why that is
	// deliberate rather than a bug. Said here rather than left to the arithmetic:
	// db.AppClockAgeMicros CLAMPS an age at a century, so the zero time would
	// otherwise be answered by a clamp rather than by the rule.
	if issuedAt.IsZero() {
		return true, nil
	}
	return !issuedAt.After(cutoff.Time) || byDBClock.Bool, nil
}

// RevokeSub stamps sub's cutoff at now, invalidating every current session
// for that principal. Idempotent (repeat revokes just move the cutoff later).
func (r *pgSessionRevocations) RevokeSub(ctx context.Context, sub string) error {
	return r.upsertCutoff(ctx, sub)
}

// RevokeAll stamps the global cutoff at now, invalidating every current
// session for every principal.
func (r *pgSessionRevocations) RevokeAll(ctx context.Context) error {
	return r.upsertCutoff(ctx, globalRevokeSub)
}

func (r *pgSessionRevocations) upsertCutoff(ctx context.Context, sub string) error {
	const q = `
		INSERT INTO oidc_session_revocations (sub, revoked_at)
		VALUES ($1, now())
		ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`
	if _, err := r.pool.Exec(ctx, q, sub); err != nil {
		return fmt.Errorf("wardynd: revoke session cutoff: %w", err)
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
// internal/auth/oidc, which must stay dependency-free of internal/types (see
// oidc.RoleMapping's own doc comment), never imports internal/store either.
// The conversion from types.RoleMapping (id/timestamps/provenance) to
// oidc.RoleMapping (bare Value/Role) happens here, the one place both types
// are in scope.
func (r *pgRoleMappings) ListRoleMappings(ctx context.Context) ([]oidc.RoleMapping, error) {
	rows, err := store.NewPG(r.pool).ListRoleMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("wardynd: list role mappings: %w", err)
	}
	out := make([]oidc.RoleMapping, len(rows))
	for i, m := range rows {
		out[i] = oidc.RoleMapping{Value: m.Value, Role: m.Role}
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

// ─── masking recorder ─────────────────────────────────────────────────────────

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
}

var _ audit.Recorder = maskingRecorder{}

func (m maskingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if m.reg != nil {
		// W20-groundtruth-mapper-2: a run-less event (ev.RunID == nil —
		// policy.inline, secret.*, an admin action) used to short-circuit this
		// WHOLE block (the old guard was `m.reg != nil && ev.RunID != nil`),
		// bypassing masking entirely instead of falling back to the
		// PROCESS-GLOBAL corpus (Bedrock SSO / subscription creds registered
		// via AddGlobal). Snapshot(uuid.Nil) returns exactly that — globals
		// only, since uuid.Nil is never a real run's perRun key — so a
		// run-less row is now masked against the same globals every real run
		// already is, just with no per-run corpus layered on top (there is
		// none to add: masking is per-run, and without a run id there is no
		// run-scoped snapshot to apply — a registered secret for some OTHER
		// run must still never leak into a run-less event's masking).
		runID := uuid.Nil
		if ev.RunID != nil {
			runID = *ev.RunID
		}
		snap := m.reg.Snapshot(runID)
		if len(snap) > 0 {
			// D31: ev.Data is JSON, so a registered secret bearing a newline,
			// quote, or backslash (e.g. a minted ssh_key PEM) lands there in its
			// JSON-escaped form, which the raw-value masker would miss. Expand the
			// snapshot with the same escaped variants the recording-upload path
			// applies to asciicast bodies before building the masker.
			masker := secretmask.NewMasker(secretmask.JSONEscapedVariants(snap))
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

// ─── lifecycle adapters ───────────────────────────────────────────────────────

// lifecycleStore adapts the function-style store package to lifecycle.Store.
// ListRunningWithPolicy reads each run's EFFECTIVE idle cap from the run row's
// auto_stop_after_sec column (captured from the resolved policy at CreateRun).
// It previously LEFT JOINed run_policies on policy_id and read the policy JSONB —
// but inline/default/scan/verify/record/harness-login runs have no stored
// policy_id, so the join was NULL and COALESCE'd to 0 (never auto-stop),
// silently exempting every such run from idle reaping.
type lifecycleStore struct {
	pool *pgxpool.Pool
}

var _ lifecycle.Store = lifecycleStore{}

func (l lifecycleStore) ListRunningWithPolicy(ctx context.Context) ([]lifecycle.RunSummary, error) {
	const q = `
		SELECT id, updated_at, auto_stop_after_sec
		FROM agent_runs
		WHERE state = $1`
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

// reapTickLock is the reaper's single-flight gate: a Postgres try-advisory-lock
// so two control planes (or an old process still winding down beside a new one)
// cannot both scan and stop the same runs on the same tick. Follows
// lifecycleStore/lifecycleStopper — the pool stays here in wardynd and lifecycle
// keeps its target-agnostic seams. A lock we cannot reach skips the tick: the
// scan that follows would fail on the same database anyway.
func reapTickLock(pool *pgxpool.Pool) func(context.Context) (func(), bool) {
	return func(ctx context.Context) (func(), bool) {
		release, ok, err := db.TryAdvisoryLock(ctx, pool, db.ReaperAdvisoryLockKey)
		if err != nil {
			slog.DebugContext(ctx, "wardynd: reap tick lock unavailable", slog.Any("err", err))
			return nil, false
		}
		return release, ok
	}
}

// groundtruthRotatorLock is the ground-truth rotator's leader-election gate
// (S2): a Postgres try-advisory-lock acquired ONCE (not per-tick, unlike
// reapTickLock above) so at most one replica runs the mint/write loop in the
// steady state while every other replica parks on a backoff. NOT a fencing
// primitive — a lost session can transiently leave two leaders; see
// runGroundtruthTokenRotatorLeader (gt_rotator.go). Unlike reapTickLock this
// surfaces err separately from a plain not-acquired so the caller can log
// standby state (lock genuinely held elsewhere) distinctly from an
// unreachable database.
func groundtruthRotatorLock(pool *pgxpool.Pool) func(context.Context) (func(), bool, error) {
	return func(ctx context.Context) (func(), bool, error) {
		return db.TryAdvisoryLock(ctx, pool, db.GroundTruthRotatorLockKey)
	}
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
			slog.ErrorContext(ctx, "wardynd: lifecycle idle-stop sandbox teardown FAILED -- sandbox may still be routable",
				slog.String("run_id", runID.String()),
				slog.Any("err", serr),
			)
			errs["teardown_error"] = serr.Error()
		}
	}
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
				slog.ErrorContext(ctx, "wardynd: approval sweep error", slog.Any("err", err))
				continue
			}
			if n > 0 {
				slog.InfoContext(ctx, "wardynd: approval sweep expired stale PENDING approvals",
					slog.Int("expired", n),
				)
			}
		}
	}
}

// runSecretSweepInterval is how often the run-secret eviction lane ticks. Well
// under api.RunSecretGrace so a cold run's corpus is dropped promptly once it
// qualifies, and cheap enough to leave unconfigured: one run listing per tick,
// and none at all while the registry holds nothing.
const runSecretSweepInterval = 15 * time.Minute

// runSecretSweeper periodically evicts the plaintext masking corpus of runs
// that have been terminal past api.RunSecretGrace, until ctx is cancelled. Same
// goroutine shape as runApprovalSweeper; the first sweep runs after one tick.
func runSecretSweeper(ctx context.Context, srv *api.Server, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := srv.SweepRunSecrets(ctx); n > 0 {
				slog.InfoContext(ctx, "wardynd: evicted masking secrets for cold terminal runs",
					slog.Int("runs", n),
				)
			}
		}
	}
}

// recordingSweepable is satisfied structurally by BOTH recording.FSStore and
// recording.PGStore. Sweep is deliberately NOT on recording.Store itself (see
// the package doc on internal/recording/store.go): retention is a
// storage-backend concern, and a future object-storage backend would use its
// bucket's own lifecycle rules instead of an app-level sweep. This unexported
// interface — rather than promoting Sweep to recording.Store, or duplicating
// the goroutine-launch code below per concrete type — is the smaller diff for
// the ONE call site (startBackgroundWorkers' type-assert in boot_serve.go)
// that needs to sweep whichever concrete store is selected.
type recordingSweepable interface {
	Sweep(olderThan time.Duration) (int, error)
}

// runRecordingSweeper periodically deletes stored session recordings older
// than `after`, until ctx is cancelled. Only started when the operator sets a
// retention window (WARDYN_RECORDING_RETENTION_DAYS); unset = keep forever,
// because a recording is governance evidence and deleting one is an operator
// decision, not a default.
//
// Deletions are audited: a sweep that removed anything emits one
// recording.retention.sweep event, so the disappearance of evidence is itself
// evidence. The first sweep runs after one tick, mirroring the other sweepers.
func runRecordingSweeper(ctx context.Context, s recordingSweepable, rec audit.Recorder, interval, after time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.Sweep(after)
			if err != nil {
				slog.ErrorContext(ctx, "wardynd: recording sweep error", slog.Any("err", err))
			}
			if n == 0 {
				continue
			}
			slog.InfoContext(ctx, "wardynd: recording sweep deleted expired recordings", slog.Int("deleted", n))
			data, _ := json.Marshal(map[string]any{"deleted": n, "retention_sec": int64(after.Seconds())})
			ev := types.AuditEvent{
				ID:        uuid.New(),
				Time:      time.Now().UTC(),
				ActorType: types.ActorSystem,
				Actor:     "wardyn/recording-sweeper",
				Action:    "recording.retention.sweep",
				Target:    "recordings",
				Outcome:   "success",
				Data:      json.RawMessage(data),
			}
			if rerr := rec.Record(ctx, ev); rerr != nil {
				audit.LogWriteFailure(ctx, ev, rerr)
			}
		}
	}
}
