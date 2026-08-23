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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// ListRunsPageByCreator is ListRunsPage narrowed to one creator, same order.
// ponytail: no dedicated (created_by, created_at) index yet — agent_runs is
// small enough per-operator that the existing created_at index plus a filter
// scan is fine; add one if a member's run list ever gets slow.
func (s PG) ListRunsPageByCreator(ctx context.Context, createdBy string, p Page) ([]types.AgentRun, error) {
	q, args := p.appendTo(`
		SELECT id, created_at, updated_at, created_by, agent, repo, task,
			policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id, title, description, workspace_ids, failure_hint
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
	q := `
		SELECT a.id, a.run_id, a.grant_id, a.kind, a.requested_scope, a.state, a.requested_at,
			a.decided_at, a.decided_by, a.minted_jti, a.reason, a.decision_scope, a.decision_expires_at
		FROM approvals a
		JOIN agent_runs r ON r.id = a.run_id
		WHERE r.created_by = $1`
	args := []any{createdBy}
	if stateFilter != "" {
		q += ` AND a.state = $2`
		args = append(args, string(stateFilter))
	}
	q += ` ORDER BY a.requested_at DESC`
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "list", "approvals by run creator", q, args, scanApproval)
}

// ListRunsPage returns runs in reverse creation order, bounded by p. The
// agent_runs_created_at_idx (0020) makes the ORDER BY + LIMIT an index scan.
func (s PG) ListRunsPage(ctx context.Context, p Page) ([]types.AgentRun, error) {
	q, args := p.appendTo(`
		SELECT id, created_at, updated_at, created_by, agent, repo, task,
			policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id, title, description, workspace_ids, failure_hint
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
		SELECT id, run_id, grant_id, kind, requested_scope, state, requested_at,
			decided_at, decided_by, minted_jti, reason, decision_scope, decision_expires_at
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
		SELECT id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data
		FROM audit_events WHERE run_id=$1 ORDER BY seq ASC`, []any{runID})
	return collect(ctx, s.Pool, "query", "audit events", q, args, scanAuditEvent)
}

// QueryRecentAuditEventsPage returns the newest-first global audit feed, bounded
// by p. seq is the audit_events PRIMARY KEY, so ORDER BY seq DESC + LIMIT is an
// index-scan-backward with no added index (see 0020's audit note).
func (s PG) QueryRecentAuditEventsPage(ctx context.Context, p Page) ([]types.AuditEvent, error) {
	q, args := p.appendTo(`
		SELECT id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data
		FROM audit_events ORDER BY seq DESC`, nil)
	return collect(ctx, s.Pool, "query", "recent audit events", q, args, scanAuditEvent)
}
