// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Pagination: the Page window, the Pager capability interface, the shared
// row-collect helper, and PG's paged read methods. Split from store.go along
// the read-surface seam — the plain unbounded List* wrappers stay next to their
// tables in store.go and delegate here with an empty Page. See Pager's doc for
// why this is NOT part of Store.

package store

import (
	"context"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Page bounds a List query to Limit rows after skipping Offset, ordered by the
// query's own ORDER BY. A zero or negative Limit means UNBOUNDED — the historical
// List* behaviour the internal callers depend on (ReconcileOnBoot's stranded-run
// scan, the create-run workspace-collision scan, the approval fan-out) all need
// the whole table, so they call the plain List* wrappers below. The public read
// handlers pass an explicit Limit (capped by api.parseListPage) via the *Page
// methods so an external client can never pull down an unbounded payload.
type Page struct {
	Limit  int
	Offset int
}

// appendTo renders " LIMIT $n [OFFSET $n+1]" onto q using positional args
// starting after the len(args) already bound, and returns the grown query plus
// args. Limit<=0 emits nothing (unbounded); OFFSET is emitted only when positive
// (offset-without-limit is meaningless for these fully-ordered feeds).
func (p Page) appendTo(q string, args []any) (string, []any) {
	if p.Limit <= 0 {
		return q, args
	}
	q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
	args = append(args, p.Limit)
	if p.Offset > 0 {
		q += fmt.Sprintf(" OFFSET $%d", len(args)+1)
		args = append(args, p.Offset)
	}
	return q, args
}

// collect runs q and scans every row with scan. verb/noun name the operation in
// the wrapped errors ("store: <verb> <noun>" on the query, "store: iterate
// <noun>" on iteration). pgx.CollectRows seeds the result with []T{} and closes
// rows itself, so the "empty, never nil" List* contract (the API's `[]`-not-
// `null` JSON) comes from the driver rather than being re-derived per call site.
func collect[T any](ctx context.Context, pool *pgxpool.Pool, verb, noun, q string, args []any, scan func(pgx.Row) (T, error)) ([]T, error) {
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s %s: %w", verb, noun, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
	if err != nil {
		return nil, fmt.Errorf("store: iterate %s: %w", noun, err)
	}
	return out, nil
}

// Pager is the paginated read surface. It is deliberately NOT part of the Store
// interface: the control plane has many test doubles that embed store.Store and
// override a handful of methods, and widening Store would silently route their
// list calls to the embedded nil interface. Handlers type-assert s.cfg.Store to
// Pager and fall back to the unbounded List* + in-Go windowing when a store (a
// test fake) does not implement it. Production always uses PG, which does.
type Pager interface {
	ListRunsPage(ctx context.Context, p Page) ([]types.AgentRun, error)
	ListPoliciesPage(ctx context.Context, p Page) ([]types.RunPolicy, error)
	ListWorkspacesPage(ctx context.Context, p Page) ([]types.Workspace, error)
	ListApprovalsPage(ctx context.Context, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error)
	QueryAuditEventsPage(ctx context.Context, runID uuid.UUID, p Page) ([]types.AuditEvent, error)
	QueryRecentAuditEventsPage(ctx context.Context, p Page) ([]types.AuditEvent, error)
	// QueryAuditEventsFilteredPage serves a NARROWED audit read (time range,
	// action, outcome, actor type); see AuditFilter in auditfilter.go. An
	// unfiltered read still uses the two methods above.
	QueryAuditEventsFilteredPage(ctx context.Context, runID *uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error)
	// ListUserDriveGrantsPage serves the drives console's allocation table. It
	// is here for the reason the six above are: one row per SUBJECT means this
	// table's size IS the deployment's headcount, and its ORDER BY has no index,
	// so unbounded it sorts every allocation on every load of one admin screen.
	ListUserDriveGrantsPage(ctx context.Context, p Page) ([]types.UserDriveGrant, error)
}

// Compile-time assertion: PG satisfies Pager (the paginated read surface).
var _ Pager = PG{}

// RunsByCreatorPager is the ownership-scoped analogue of Pager.ListRunsPage: a
// member's GET /runs is scoped to created_by = the caller (internal/api's
// isOperator decides who is a member). Kept OUT of Pager for the same reason
// Pager is kept out of Store (widening either silently reroutes a test fake's
// embedded-but-not-overridden method to the wrong behavior) — AND for a second,
// stronger reason specific to this one: Pager's own absence falls back to a
// SAFE fetch-all + in-Go window (nothing is scoped, so an unscoped fallback
// changes nothing). This interface's absence must never fall back that way — an
// unscoped list IS the vulnerability for a member — so the api-layer call site
// fails closed (a clear 500) when a store does not implement it, rather than
// silently serving every run.
type RunsByCreatorPager interface {
	ListRunsPageByCreator(ctx context.Context, createdBy string, p Page) ([]types.AgentRun, error)
}

// Compile-time assertion: PG satisfies RunsByCreatorPager.
var _ RunsByCreatorPager = PG{}

// ActiveRunsByCreatorReader answers the ONE question a new sign-in asks before
// it launches: which of THIS person's runs of this lane are still live, so the
// supersede can end them (supersedeCallerLoginRuns, internal/api).
//
// A capability interface for ActiveRunsAtPathReader's reasons, and answered the
// same way — a WHERE clause, not a window — but its ABSENCE is handled
// differently from either neighbour: the api-layer call site (liveLoginRunsBy)
// takes NO fallback at all. It does not widen anything the way an unscoped list
// would (RunsByCreatorPager's fail-closed case), and it does not fall back to
// the unbounded ListRuns the way ActiveRunsAtPathReader does, because this runs
// on a route every store-less embedding drives and the scan would read a whole
// run table to find at most one row. A store without this method simply does
// not supersede; PG is the production store and implements it, and the capture
// upload's own KILLED guard is the belt.
//
// task and agent, not "provider": a run row carries no provider column (the
// provider lives in the harness.login.started audit datum), and task+agent is
// what actually separates one lane's login box from another's.
type ActiveRunsByCreatorReader interface {
	ActiveRunsByCreator(ctx context.Context, createdBy, task, agent string) ([]types.AgentRun, error)
}

// Compile-time assertion: PG satisfies ActiveRunsByCreatorReader.
var _ ActiveRunsByCreatorReader = PG{}

// ActiveRunsByCreator returns createdBy's non-terminal runs of one task+agent.
//
// The state predicate is the POSITIVE list (types.NonTerminalRunStates), the
// choice CountActiveRunsBy and ActiveRunsAtWorkspacePath both make and for the
// same reason: a state added to the enum and forgotten here merely misses a
// supersede, while `NOT IN (terminal)` would hand a newly-added TERMINAL state
// to the kill cascade.
//
// ponytail: no new index. agent_runs_created_by_idx already indexes the
// selective column and one person owns few runs; a composite is the upgrade if
// a deployment ever has a member with enough history to notice.
func (s PG) ActiveRunsByCreator(ctx context.Context, createdBy, task, agent string) ([]types.AgentRun, error) {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	q := `SELECT ` + runCols + ` FROM agent_runs
		WHERE created_by = $1 AND task = $2 AND agent = $3 AND state = ANY($4)
		ORDER BY created_at DESC`
	return collect(ctx, s.Pool, "list", "active runs by creator", q, []any{createdBy, task, agent, states}, scanRun)
}

// LoginLocker serializes ONE person's sign-in launches, and the credential
// capture that follows one, against every other replica — the piece the
// deterministic tie-break in api.supersedeOlderLoginRuns could not supply.
//
// A capability interface for ActiveRunsByCreatorReader's reasons (widening
// Store would make every double and embedding in the tree implement a lock they
// are not about), and its ABSENCE is handled the same way that neighbour's is:
// the call site takes NO fallback and proceeds UNLOCKED — a store that cannot
// lock is exactly as serialized as 0.7.8 was. PG is the production store and
// implements it, so this arm is reached only by test doubles. A lock that
// EXISTS but cannot be taken in time is a refusal, not this.
//
// Keyed by ACTOR — the login run's creator — not by the credential scope: under
// the `shared` roster every sign-in resolves to the same empty scope owner, so
// a scope-keyed lock would serialize the whole deployment while serializing the
// one thing it needs to (one person's two launches) no better.
type LoginLocker interface {
	LockLoginSupersede(ctx context.Context, actor string) (release func(), err error)
}

// Compile-time assertion: PG satisfies LoginLocker.
var _ LoginLocker = PG{}

// ErrLoginLockNoCapacity is the one LockLoginSupersede error a caller may
// proceed unlocked on: the pool cannot spare a connection for the hold (always
// true at the documented pool floor). Any other error is a wait that expired
// or a database fault, and the caller refuses rather than run unserialized.
var ErrLoginLockNoCapacity = db.ErrAdvisoryLockNoCapacity

// LockLoginSupersede holds db.LoginSupersedeLockClass keyed to actor for up to
// db.LoginSupersedeLockWait. Session-scoped, not transaction-scoped: the work it
// guards is several independent statements (two supersede passes around a run
// insert) and, on the capture path, a read-modify-write in a LATER request.
//
// It borrows from THIS pool — the request-serving one — so the cost is stated
// where an operator sizing pool_max_conns can find it: one connection for the
// duration of one hold, at most one per process at a time, and none at all
// when the pool cannot spare two (db.AdvisoryLockKeyed). An error means the
// lock was not taken; see ErrLoginLockNoCapacity for which one a caller may
// proceed on.
func (s PG) LockLoginSupersede(ctx context.Context, actor string) (func(), error) {
	return db.AdvisoryLockKeyed(ctx, s.Pool, db.LoginSupersedeLockClass, loginLockObject(actor), db.LoginSupersedeLockWait)
}

// loginLockObject folds an actor string into the objid half of the key, in ONE
// place so the launch and the capture can never disagree about which lock a
// person's sign-in takes.
//
// crc32, deliberately not a cryptographic digest: nothing here is a secret or a
// capability, and a COLLISION is harmless by construction — two people whose
// actor strings collide merely take the same lock and over-serialize each
// other's sign-ins by a few statements. It is not a correctness risk, only a
// contention one, and one nobody will ever measure.
func loginLockObject(actor string) int32 {
	return int32(crc32.ChecksumIEEE([]byte(actor)))
}

// ActiveRunsAtPathReader answers ONE question the create path asks on every run:
// which OTHER non-terminal runs already operate on this host workspace path.
//
// It is a capability interface for Pager's reason, and it is separate from
// Pager for RunsByCreatorPager's: this one is answered by a WHERE clause rather
// than a window, so a store that does not implement it cannot be served by
// windowing the unbounded list. The api-layer call site falls back to the
// unbounded ListRuns + an in-Go filter, which is what it did before this
// existed — SAFE here, unlike the ownership-scoped list, because the answer is
// identical either way and the fallback is merely slower.
//
// It exists because, without it, finding the handful of runs sharing one path
// means loading EVERY run in the deployment — a Seq Scan plus a full sort of
// agent_runs, on every single run create, over a table nothing prunes and no
// retention policy bounds. The warning is advisory and never blocks a launch,
// so sorting a deployment's whole run history is not worth paying for a
// sentence that is usually not printed.
type ActiveRunsAtPathReader interface {
	ActiveRunsAtWorkspacePath(ctx context.Context, workspacePath string) ([]types.AgentRun, error)
}

// Compile-time assertion: PG satisfies ActiveRunsAtPathReader.
var _ ActiveRunsAtPathReader = PG{}

// ActiveRunsAtWorkspacePath returns the non-terminal runs bound to one host
// workspace path.
//
// The state predicate is the POSITIVE list (types.NonTerminalRunStates), the
// same choice CountActiveRunsBy makes and for the same reason: a state added to
// the enum and forgotten there merely under-warns, while `NOT IN (terminal)`
// would treat a newly-added TERMINAL state as active and warn about a
// collision with a run that finished.
//
// ponytail: no new index. workspace_path is selective and the state filter is a
// cheap check over the rows that match it; a composite (workspace_path, state)
// index is the upgrade if a deployment ever has enough runs on ONE path to
// notice. What this replaces was not an index problem — it was reading the
// whole table.
func (s PG) ActiveRunsAtWorkspacePath(ctx context.Context, workspacePath string) ([]types.AgentRun, error) {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	q := `SELECT ` + runCols + ` FROM agent_runs WHERE workspace_path = $1 AND state = ANY($2) ORDER BY created_at DESC`
	return collect(ctx, s.Pool, "list", "active runs at workspace path", q, []any{workspacePath, states}, scanRun)
}

// ListRunsPageByCreator is ListRunsPage narrowed to one creator, same order.
// ponytail: no dedicated (created_by, created_at) index yet — agent_runs is
// small enough per-operator that the existing created_at index plus a filter
// scan is fine; add one if a member's run list ever gets slow.
func (s PG) ListRunsPageByCreator(ctx context.Context, createdBy string, p Page) ([]types.AgentRun, error) {
	q, args := p.appendTo(`
		SELECT `+runCols+`
		FROM agent_runs WHERE created_by = $1 ORDER BY created_at DESC`, []any{createdBy})
	return collect(ctx, s.Pool, "list", "runs by creator", q, args, scanRun)
}

// ApprovalsByRunCreatorPager is the ownership-scoped analogue of
// Pager.ListApprovalsPage: a member's GET /approvals (no ?run_id=) is scoped to
// approvals raised on runs THEY created. Approvals carry no created_by of their
// own (they belong to a run, not a human directly), so this JOINs agent_runs.
// Same fail-closed contract as RunsByCreatorPager — an absent implementation
// must never fall back to the unscoped list.
//
// api.Config.Approvals is wardynd's approvalService wrapper
// (cmd/wardynd/adapters.go), not a bare store.PG, so it needs its own
// delegation method for this to be reachable in production — it has one, and
// asserts the interface, so a member's unscoped GET /approvals is served from
// the store rather than the api-layer fail-closed fallback.
type ApprovalsByRunCreatorPager interface {
	ListApprovalsPageByRunCreator(ctx context.Context, createdBy string, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error)
}

// Compile-time assertion: PG satisfies ApprovalsByRunCreatorPager.
var _ ApprovalsByRunCreatorPager = PG{}

// ListApprovalsPageByRunCreator is ListApprovalsPage narrowed to approvals on
// runs createdBy owns, via a JOIN on agent_runs (approvals has no created_by of
// its own). Same state filter and ordering as ListApprovalsPage.
func (s PG) ListApprovalsPageByRunCreator(ctx context.Context, createdBy string, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error) {
	// approvalCols SPLICED, not a thirteenth copy of the column list. This was
	// the one approvals reader that hand-wrote its columns — because the JOIN
	// form needed every one of them prefixed `a.` — and scanApproval is shared,
	// so the next column APPENDED to approvalCols (the documented way to add
	// one) would land in every admin path and not in this one: the MEMBER's
	// unscoped GET /approvals alone 500s on scan arity, with every gate green.
	// The semi-join reaches the same rows with no alias to prefix, so the const
	// goes in verbatim and there is nothing left to keep in step by hand.
	q := `
		SELECT ` + approvalCols + `
		FROM approvals
		WHERE run_id IN (SELECT id FROM agent_runs WHERE created_by = $1)`
	args := []any{createdBy}
	if stateFilter != "" {
		q += ` AND state = $2`
		args = append(args, string(stateFilter))
	}
	q += ` ORDER BY requested_at DESC`
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "list", "approvals by run creator", q, args, scanApproval)
}

// ApprovalsByRunPager is Pager.ListApprovalsPage narrowed to ONE run — the
// ?run_id= shape of GET /api/v1/approvals, which the CLI and the console's run
// detail page poll. A capability interface for Pager's reason.
//
// It exists because a run-scoped request with no dedicated reader falls to the
// fetch-all branch: Approvals.List -> store.ListApprovals ->
// ListApprovalsPage(ctx, state, Page{}) -> a Page with Limit<=0, which emits
// NO LIMIT clause at all. One run-scoped poll therefore materialises EVERY
// approval row the deployment has ever written, in Go, and discards all but
// one run's. Decided rows are never deleted, so that read grows with
// deployment age — the same cost ListApprovalsPage already removes for the
// unfiltered list.
//
// Same fail-safe contract as Pager (NOT ApprovalsByRunCreatorPager's fail-CLOSED
// one): an absent implementation falls back to the fetch-all + in-Go filter,
// which returns the identical rows and is merely slower. Ownership scoping is
// decided BEFORE this is reached (getRunAuthorized), so the fallback is not a
// privilege question.
type ApprovalsByRunPager interface {
	ListApprovalsPageByRun(ctx context.Context, runID uuid.UUID, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error)
}

// Compile-time assertion: PG satisfies ApprovalsByRunPager.
var _ ApprovalsByRunPager = PG{}

// ListApprovalsPageByRun is ListApprovalsPage narrowed to one run, same state
// filter and ordering.
//
// ponytail: no new index. 0001's approvals_run_idx (run_id) already makes this
// an index scan, and a single run's approvals are few enough that sorting them
// by requested_at is free — the cost this removes was never a missing index, it
// was reading the whole table. A composite (run_id, requested_at DESC) is the
// upgrade if one run ever accumulates enough approvals to notice.
func (s PG) ListApprovalsPageByRun(ctx context.Context, runID uuid.UUID, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error) {
	q := `
		SELECT ` + approvalCols + `
		FROM approvals WHERE run_id = $1`
	args := []any{runID}
	if stateFilter != "" {
		q += ` AND state = $2`
		args = append(args, string(stateFilter))
	}
	q += ` ORDER BY requested_at DESC`
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "list", "approvals by run", q, args, scanApproval)
}

// ListRunsPage returns runs in reverse creation order, bounded by p. The
// agent_runs_created_at_idx (0020) makes the ORDER BY + LIMIT an index scan.
func (s PG) ListRunsPage(ctx context.Context, p Page) ([]types.AgentRun, error) {
	q, args := p.appendTo(`
		SELECT `+runCols+`
		FROM agent_runs ORDER BY created_at DESC`, nil)
	return collect(ctx, s.Pool, "list", "runs", q, args, scanRun)
}

// ListPoliciesPage returns policies in reverse creation order, bounded by p.
// run_policies_created_at_idx (0023) covers the ORDER BY.
func (s PG) ListPoliciesPage(ctx context.Context, p Page) ([]types.RunPolicy, error) {
	q, args := p.appendTo(`SELECT id, name, created_at, updated_at, spec FROM run_policies ORDER BY created_at DESC`, nil)
	return collect(ctx, s.Pool, "list", "policies", q, args, scanPolicy)
}

// ListWorkspacesPage returns workspaces in reverse creation order, bounded by p.
// workspaces_created_at_idx (0023) covers the ORDER BY.
func (s PG) ListWorkspacesPage(ctx context.Context, p Page) ([]types.Workspace, error) {
	q, args := p.appendTo(`SELECT `+wsCols+` FROM workspaces ORDER BY created_at DESC`, nil)
	wss, err := collect(ctx, s.Pool, "list", "workspaces", q, args, scanWorkspace)
	if err != nil {
		return nil, err
	}
	// Bulk hydrate: ONE sources query for the union across the whole page —
	// referencedWorkspaces full-lists on run-create/preflight, so per-row
	// hydration would multiply a hot path.
	return s.hydrateAll(ctx, wss)
}

// WorkspacesByOwnerPager is the ownership-scoped analogue of
// Pager.ListWorkspacesPage: a MEMBER's GET /workspaces sees their OWN owned rows
// plus the operator-owned ones (owned_by = ”), never another member's.
//
// Unlike RunsByCreatorPager, an absent implementation here is NOT a
// fail-closed case: the api-layer fallback fetches all and applies the SAME
// owned_by filter in Go before windowing, so the scoping still holds — only the
// LIMIT/OFFSET moves out of the database. Kept out of Pager for the usual
// reason (a test fake embedding Store must not silently inherit it).
type WorkspacesByOwnerPager interface {
	ListWorkspacesPageForOwner(ctx context.Context, owner string, p Page) ([]types.Workspace, error)
}

// Compile-time assertion: PG satisfies WorkspacesByOwnerPager.
var _ WorkspacesByOwnerPager = PG{}

// ListWorkspacesPageForOwner is ListWorkspacesPage narrowed to what one member
// may see: their own owned rows plus every operator-owned row (” — the 0048
// default, i.e. every pre-0.6 workspace). workspaces_owned_by_idx (0048) covers
// the IN.
func (s PG) ListWorkspacesPageForOwner(ctx context.Context, owner string, p Page) ([]types.Workspace, error) {
	q, args := p.appendTo(
		`SELECT `+wsCols+` FROM workspaces WHERE owned_by IN ('', $1) ORDER BY created_at DESC`,
		[]any{owner})
	wss, err := collect(ctx, s.Pool, "list", "workspaces by owner", q, args, scanWorkspace)
	if err != nil {
		return nil, err
	}
	return s.hydrateAll(ctx, wss)
}

// ListApprovalsPage returns approvals filtered by state (empty = all) in reverse
// request order, bounded by p. The all-state feed rides approvals_requested_at_idx
// (0020); a single-state filter rides approvals_state_requested_at_idx (0023),
// which serves both the WHERE and the ORDER BY without a sort.
func (s PG) ListApprovalsPage(ctx context.Context, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error) {
	q := `
		SELECT ` + approvalCols + `
		FROM approvals`
	var args []any
	if stateFilter != "" {
		q += ` WHERE state=$1`
		args = append(args, string(stateFilter))
	}
	q += ` ORDER BY requested_at DESC`
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "list", "approvals", q, args, scanApproval)
}

// QueryAuditEventsPage returns a run's audit events in seq (chronological) order,
// bounded by p. audit_events_run_seq_idx (0023) makes WHERE run_id + ORDER BY seq
// an indexed range scan with no sort; OFFSET pages forward without flipping to
// DESC, so the per-run trail stays ASC (docs/sdk.md's exit-code contract) and a
// caller pages to the newest events with ?offset=.
func (s PG) QueryAuditEventsPage(ctx context.Context, runID uuid.UUID, p Page) ([]types.AuditEvent, error) {
	q, args := p.appendTo(`
		SELECT `+auditCols+`
		FROM audit_events WHERE run_id=$1 ORDER BY seq ASC`, []any{runID})
	return collect(ctx, s.Pool, "query", "audit events", q, args, scanAuditEvent)
}

// QueryRecentAuditEventsPage returns the newest-first global audit feed, bounded
// by p. seq is the audit_events PRIMARY KEY, so ORDER BY seq DESC + LIMIT is an
// index-scan-backward with no added index (see 0020's audit note).
func (s PG) QueryRecentAuditEventsPage(ctx context.Context, p Page) ([]types.AuditEvent, error) {
	q, args := p.appendTo(`
		SELECT `+auditCols+`
		FROM audit_events ORDER BY seq DESC`, nil)
	return collect(ctx, s.Pool, "query", "recent audit events", q, args, scanAuditEvent)
}

// AWSSSOSpentTokenStore persists the AWS SSO refresh-token "spent" mark
// (internal/api/awssso_refresh.go's ssoRefreshSpent map) so it survives a
// daemon restart. A capability interface for the usual reason (widening Store
// would silently route a test double's embedded-but-not-overridden methods to
// the wrong behavior), and for a second one specific to this seam: the mark is
// written precisely BECAUSE the secret/blob store (storeAWSSSOBlob) failed, so
// its own persistence must not depend on that same store — it goes through
// Store/PG (Postgres) instead, a store the blob write's own failure says
// nothing about.
type AWSSSOSpentTokenStore interface {
	// MarkAWSSSOTokenSpent upserts one spent-token row. Idempotent: marking an
	// already-spent fingerprint again touches nothing (the ON CONFLICT is a
	// no-op, not a marked_at bump) — the row's age is "since first spent", which
	// is what the reaper-tick prune below measures against.
	MarkAWSSSOTokenSpent(ctx context.Context, fingerprint, owner string, markedAt time.Time) error
	// AWSSSOTokenSpent reports whether fingerprint has a row — i.e. whether this
	// refresh token is already known dead. Read-once by its one caller
	// (Server.awsSSOTokenSpent memoizes the answer into the in-memory map after
	// the first read of a given fingerprint), so a cache miss costs at most one
	// query per fingerprint per process lifetime.
	AWSSSOTokenSpent(ctx context.Context, fingerprint string) (bool, error)
	// PruneAWSSSOSpentTokens deletes rows marked before cutoff and reports how
	// many it removed. Called from the lifecycle reaper's existing per-tick
	// advisory lock (cmd/wardynd's reapTickLock) rather than a new timer.
	PruneAWSSSOSpentTokens(ctx context.Context, cutoff time.Time) (int, error)
}

// Compile-time assertion: PG satisfies AWSSSOSpentTokenStore.
var _ AWSSSOSpentTokenStore = PG{}

// MarkAWSSSOTokenSpent upserts the spent-token row. ON CONFLICT DO NOTHING: a
// fingerprint already marked spent stays marked from its FIRST sighting, so a
// second failed persist (or a second AWS invalid_grant on the same token)
// never resets the clock the prune measures age against.
func (s PG) MarkAWSSSOTokenSpent(ctx context.Context, fingerprint, owner string, markedAt time.Time) error {
	const q = `INSERT INTO aws_sso_spent_tokens (fingerprint, owner, marked_at) VALUES ($1, $2, $3)
		ON CONFLICT (fingerprint) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, fingerprint, owner, markedAt); err != nil {
		return fmt.Errorf("store: mark aws sso token spent: %w", err)
	}
	return nil
}

// AWSSSOTokenSpent reports whether fingerprint has a persisted spent-token row.
func (s PG) AWSSSOTokenSpent(ctx context.Context, fingerprint string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM aws_sso_spent_tokens WHERE fingerprint = $1)`
	var spent bool
	if err := s.Pool.QueryRow(ctx, q, fingerprint).Scan(&spent); err != nil {
		return false, fmt.Errorf("store: read aws sso token spent: %w", err)
	}
	return spent, nil
}

// PruneAWSSSOSpentTokens deletes spent-token rows older than cutoff.
// aws_sso_spent_tokens_marked_at_idx (0068) backs the WHERE clause.
func (s PG) PruneAWSSSOSpentTokens(ctx context.Context, cutoff time.Time) (int, error) {
	const q = `DELETE FROM aws_sso_spent_tokens WHERE marked_at < $1`
	tag, err := s.Pool.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune aws sso spent tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
