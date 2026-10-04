// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Pagination: the Page window, the Pager capability interface, the shared
// row-collect helper, and PG's paged read methods; store.go's unbounded
// List* wrappers delegate here with an empty Page. See Pager's doc for why
// this isn't part of Store.

package store

import (
	"context"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Page bounds a List query to Limit rows after Offset, ordered by the
// query's own ORDER BY. Limit<=0 means unbounded, used by internal callers
// needing the whole table; public read handlers always pass an explicit
// Limit (capped by api.parseListPage).
type Page struct {
	Limit  int
	Offset int
}

// appendTo appends " LIMIT $n [OFFSET $n+1]" to q, using positional args
// after those already bound. Limit<=0 emits nothing (unbounded); OFFSET is
// emitted only when positive (meaningless without a limit here).
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

// collect runs q and scans every row with scan, wrapping errors as
// "store: <verb> <noun>" (query) or "store: iterate <noun>" (scan). Always
// returns []T{}, never nil, matching the API's empty-array contract.
func collect[T any](ctx context.Context, pool Querier, verb, noun, q string, args []any, scan func(pgx.Row) (T, error)) ([]T, error) {
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

// Pager is the paginated read surface, deliberately excluded from Store:
// widening Store would silently route a test double's embedded-but-not-
// overridden methods to it. Handlers type-assert for Pager, falling back to
// unbounded List* + in-Go windowing when a store doesn't implement it; PG
// always does.
type Pager interface {
	ListRunsPage(ctx context.Context, p Page) ([]types.AgentRun, error)
	ListPoliciesPage(ctx context.Context, p Page) ([]types.RunPolicy, error)
	ListWorkspacesPage(ctx context.Context, p Page) ([]types.Workspace, error)
	ListApprovalsPage(ctx context.Context, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error)
	QueryAuditEventsPage(ctx context.Context, runID uuid.UUID, p Page) ([]types.AuditEvent, error)
	QueryRecentAuditEventsPage(ctx context.Context, p Page) ([]types.AuditEvent, error)
	// QueryAuditEventsFilteredPage serves a narrowed audit read (time range,
	// action, outcome, actor type); see AuditFilter in auditfilter.go.
	QueryAuditEventsFilteredPage(ctx context.Context, runID *uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error)
	// ListUserDriveGrantsPage serves the drives console's allocation table;
	// its ORDER BY has no index, so unbounded it sorts every allocation on
	// every load of the admin screen.
	ListUserDriveGrantsPage(ctx context.Context, p Page) ([]types.UserDriveGrant, error)
}

var _ Pager = PG{}

// RunsByCreatorPager is the ownership-scoped analogue of Pager.ListRunsPage:
// a member's GET /runs is scoped to created_by = the caller. TRUST BOUNDARY:
// unlike Pager, an absent implementation must fail closed (500), not fall
// back to the unscoped list — an unscoped list IS the vulnerability here.
type RunsByCreatorPager interface {
	ListRunsPageByCreator(ctx context.Context, createdBy string, p Page) ([]types.AgentRun, error)
}

var _ RunsByCreatorPager = PG{}

// ActiveRunsByCreatorReader answers which of a person's runs of one
// task+agent are still non-terminal, so a new sign-in's supersede
// (supersedeCallerLoginRuns) can end them. Its absence has no fallback; PG
// implements it in production. task and agent, not "provider": a run row
// carries no provider column (it lives in the harness.login.start audit datum).
type ActiveRunsByCreatorReader interface {
	ActiveRunsByCreator(ctx context.Context, createdBy, task, agent string) ([]types.AgentRun, error)
}

var _ ActiveRunsByCreatorReader = PG{}

// ActiveRunsByCreator returns createdBy's non-terminal runs of one
// task+agent. Uses the POSITIVE state list (types.NonTerminalRunStates): a
// state added to the enum and forgotten here merely misses a supersede,
// whereas `NOT IN (terminal)` would hand a new terminal state to the kill cascade.
//
// ponytail: no new index. agent_runs_created_by_idx already indexes the
// selective column and one person owns few runs; upgrade to a composite if
// a deployment ever has enough history to notice.
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

// LoginLocker serializes one person's sign-in launches, and the credential
// capture that follows one, against every other replica — the piece the
// deterministic tie-break in api.supersedeOlderLoginRuns couldn't supply.
// Its absence falls back UNLOCKED (as serialized as 0.7.8 was); PG
// implements it in production.
//
// Keyed by ACTOR, not credential scope: under the `shared` roster every
// sign-in resolves to the same empty scope owner, so a scope-keyed lock
// would serialize the whole deployment without serializing what it needs to.
type LoginLocker interface {
	LockLoginSupersede(ctx context.Context, actor string) (release func(), err error)
}

var _ LoginLocker = PG{}

// ErrLoginLockNoCapacity is the one LockLoginSupersede error a caller may
// proceed unlocked on (the pool can't spare a connection); any other error
// means the caller should refuse.
var ErrLoginLockNoCapacity = db.ErrAdvisoryLockNoCapacity

// LockLoginSupersede holds db.LoginSupersedeLockClass keyed to actor for up
// to db.LoginSupersedeLockWait. Session-scoped, not transaction-scoped: it
// guards several independent statements, including a later read-modify-write
// on the capture path. Borrows one connection from this pool for the hold's
// duration; see ErrLoginLockNoCapacity for the one error a caller may proceed on.
func (s PG) LockLoginSupersede(ctx context.Context, actor string) (func(), error) {
	return db.AdvisoryLockKeyed(ctx, s.Pool, db.LoginSupersedeLockClass, loginLockObject(actor), db.LoginSupersedeLockWait)
}

// loginLockObject folds an actor string into the key's objid half, in one
// place so launch and capture agree on which lock a sign-in takes. crc32,
// not cryptographic: a collision is harmless, merely over-serializing two
// colliding actors' sign-ins.
func loginLockObject(actor string) int32 {
	return int32(crc32.ChecksumIEEE([]byte(actor)))
}

// ActiveRunsAtPathReader answers which OTHER non-terminal runs already
// operate on one host workspace path, checked on every run create. Its
// absence falls back to the unbounded ListRuns + an in-Go filter, SAFE
// (same answer, merely slower) since the warning is advisory and never
// blocks a launch.
type ActiveRunsAtPathReader interface {
	ActiveRunsAtWorkspacePath(ctx context.Context, workspacePath string) ([]types.AgentRun, error)
}

var _ ActiveRunsAtPathReader = PG{}

// ActiveRunsAtWorkspacePath returns the non-terminal runs bound to one host
// workspace path. Uses the POSITIVE state list, same reasoning as
// ActiveRunsByCreator: a forgotten new state merely under-warns rather than
// mis-treating it as active.
//
// ponytail: no new index. workspace_path is selective and the state filter
// is a cheap check over matching rows; a composite (workspace_path, state)
// index is the upgrade if a deployment ever has enough runs on ONE path.
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
// small enough per-operator that the existing created_at index plus a
// filter scan is fine; add one if a member's run list ever gets slow.
func (s PG) ListRunsPageByCreator(ctx context.Context, createdBy string, p Page) ([]types.AgentRun, error) {
	q, args := p.appendTo(`
		SELECT `+runCols+`
		FROM agent_runs WHERE created_by = $1 ORDER BY created_at DESC`, []any{createdBy})
	return collect(ctx, s.Pool, "list", "runs by creator", q, args, scanRun)
}

// ApprovalsByRunCreatorPager is the ownership-scoped analogue of
// Pager.ListApprovalsPage: a member's GET /approvals (no ?run_id=) is scoped
// to approvals on runs THEY created, via a JOIN on agent_runs (approvals
// carry no created_by of their own). TRUST BOUNDARY: same fail-closed
// contract as RunsByCreatorPager — an absent implementation must never fall
// back to the unscoped list.
type ApprovalsByRunCreatorPager interface {
	ListApprovalsPageByRunCreator(ctx context.Context, createdBy string, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error)
}

var _ ApprovalsByRunCreatorPager = PG{}

// ListApprovalsPageByRunCreator is ListApprovalsPage narrowed to approvals on
// runs createdBy owns, via a JOIN on agent_runs (approvals has no created_by
// of its own). Same state filter and ordering as ListApprovalsPage.
func (s PG) ListApprovalsPageByRunCreator(ctx context.Context, createdBy string, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error) {
	// approvalCols spliced verbatim (not hand-copied): the semi-join needs no
	// `a.` alias prefix, so a column appended to approvalCols stays in sync
	// automatically — a hand-copy would silently drift and 500 only on this
	// member path.
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

// ApprovalsByRunPager is Pager.ListApprovalsPage narrowed to ONE run, the
// ?run_id= shape of GET /api/v1/approvals. Without it, a run-scoped poll
// falls back to fetching every approval the deployment has ever written and
// filtering in Go, a cost growing with deployment age since decided rows are
// never deleted.
//
// Fail-safe like Pager (not fail-closed like ApprovalsByRunCreatorPager):
// ownership scoping happens before this is reached (getRunAuthorized), so
// the fallback is a performance question, not a privilege one.
type ApprovalsByRunPager interface {
	ListApprovalsPageByRun(ctx context.Context, runID uuid.UUID, stateFilter types.ApprovalState, p Page) ([]types.ApprovalRequest, error)
}

var _ ApprovalsByRunPager = PG{}

// ListApprovalsPageByRun is ListApprovalsPage narrowed to one run, same
// state filter and ordering.
//
// ponytail: no new index. approvals_run_idx already makes this an index
// scan, and one run's approvals are few enough that sorting by requested_at
// is free — upgrade to a composite (run_id, requested_at DESC) if that changes.
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

// ListRunsPage returns runs in reverse creation order, bounded by p.
// agent_runs_created_at_idx makes the ORDER BY + LIMIT an index scan.
func (s PG) ListRunsPage(ctx context.Context, p Page) ([]types.AgentRun, error) {
	q, args := p.appendTo(`
		SELECT `+runCols+`
		FROM agent_runs ORDER BY created_at DESC`, nil)
	return collect(ctx, s.Pool, "list", "runs", q, args, scanRun)
}

// ListPoliciesPage returns policies in reverse creation order, bounded by p
// (run_policies_created_at_idx covers the ORDER BY).
func (s PG) ListPoliciesPage(ctx context.Context, p Page) ([]types.RunPolicy, error) {
	q, args := p.appendTo(`SELECT id, name, created_at, updated_at, spec FROM run_policies ORDER BY created_at DESC`, nil)
	return collect(ctx, s.Pool, "list", "policies", q, args, scanPolicy)
}

// ListWorkspacesPage returns workspaces in reverse creation order, bounded
// by p (workspaces_created_at_idx covers the ORDER BY).
func (s PG) ListWorkspacesPage(ctx context.Context, p Page) ([]types.Workspace, error) {
	q, args := p.appendTo(`SELECT `+wsCols+` FROM workspaces ORDER BY created_at DESC`, nil)
	wss, err := collect(ctx, s.Pool, "list", "workspaces", q, args, scanWorkspace)
	if err != nil {
		return nil, err
	}
	// Bulk hydrate in one query across the page; per-row hydration would
	// multiply a hot path (referencedWorkspaces full-lists on run-create).
	return s.hydrateAll(ctx, wss)
}

// WorkspacesByOwnerPager is the ownership-scoped analogue of
// Pager.ListWorkspacesPage: a member's GET /workspaces sees their own owned
// rows plus the operator-owned ones (owned_by = ”), never another member's.
// Unlike RunsByCreatorPager, an absent implementation is not fail-closed:
// the api-layer fallback applies the same owned_by filter in Go before
// windowing, so scoping still holds.
type WorkspacesByOwnerPager interface {
	ListWorkspacesPageForOwner(ctx context.Context, owner string, p Page) ([]types.Workspace, error)
}

var _ WorkspacesByOwnerPager = PG{}

// ListWorkspacesPageForOwner is ListWorkspacesPage narrowed to what one
// member may see: their own owned rows plus every operator-owned row (”,
// every pre-0.6 workspace). workspaces_owned_by_idx covers the IN.
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

// GrantsByRunPager is the scoped analogue of Pager for GET /runs/{id}/grants:
// ListGrantsByRun is already WHERE run_id=$1, so an absent implementation is
// safe (not fail-closed) — the fallback windows the same scoped rows in Go.
type GrantsByRunPager interface {
	ListGrantsByRunPage(ctx context.Context, runID uuid.UUID, p Page) ([]types.CredentialGrant, error)
}

var _ GrantsByRunPager = PG{}

// ListGrantsByRunPage is ListGrantsByRun bounded by p.
func (s PG) ListGrantsByRunPage(ctx context.Context, runID uuid.UUID, p Page) ([]types.CredentialGrant, error) {
	q, args := p.appendTo(`SELECT id, run_id, created_at, spec FROM credential_grants WHERE run_id=$1 ORDER BY created_at, id`, []any{runID})
	return collect(ctx, s.Pool, "list", "grants", q, args, scanGrant)
}

// SSHKeysByPrincipalPager is the scoped analogue of Pager for GET
// /me/ssh-keys, same safe-fallback posture as GrantsByRunPager
// (ListSSHKeysByPrincipal is already WHERE principal=$1).
type SSHKeysByPrincipalPager interface {
	ListSSHKeysByPrincipalPage(ctx context.Context, principal string, p Page) ([]types.SSHPublicKey, error)
}

var _ SSHKeysByPrincipalPager = PG{}

// ListSSHKeysByPrincipalPage is ListSSHKeysByPrincipal bounded by p.
func (s PG) ListSSHKeysByPrincipalPage(ctx context.Context, principal string, p Page) ([]types.SSHPublicKey, error) {
	q, args := p.appendTo(`SELECT `+sshKeyCols+` FROM ssh_public_keys WHERE principal = $1 ORDER BY created_at DESC, fingerprint`, []any{principal})
	return collect(ctx, s.Pool, "list", "ssh keys", q, args, scanSSHKey)
}

// APITokensByPrincipalPager is the scoped analogue of Pager for GET
// /me/tokens, same safe-fallback posture as GrantsByRunPager
// (ListAPITokensByPrincipal is already WHERE principal=$1).
type APITokensByPrincipalPager interface {
	ListAPITokensByPrincipalPage(ctx context.Context, principal string, p Page) ([]types.APIToken, error)
}

var _ APITokensByPrincipalPager = PG{}

// ListAPITokensByPrincipalPage is ListAPITokensByPrincipal bounded by p.
func (s PG) ListAPITokensByPrincipalPage(ctx context.Context, principal string, p Page) ([]types.APIToken, error) {
	q, args := p.appendTo(`SELECT `+apiTokenCols+` FROM api_tokens WHERE principal = $1 ORDER BY created_at DESC, id`, []any{principal})
	return queryAPITokens(ctx, s, q, args...)
}

// CapabilityGrantsForPager is the scoped analogue of Pager for GET
// /me/capabilities, same safe-fallback posture as GrantsByRunPager; exists
// for the uniform list-route contract (?limit=&offset=+X-Wardyn-Truncated),
// not because grant lists routinely truncate.
type CapabilityGrantsForPager interface {
	ListCapabilityGrantsForPage(ctx context.Context, users, groups []string, userType string, p Page) ([]types.CapabilityGrant, error)
}

var _ CapabilityGrantsForPager = PG{}

// ListCapabilityGrantsForPage is ListCapabilityGrantsFor bounded by p: same
// four subject arms, so a page never drops a `user_type` grant the unpaged
// read returns.
func (s PG) ListCapabilityGrantsForPage(ctx context.Context, users, groups []string, userType string, p Page) ([]types.CapabilityGrant, error) {
	if users == nil {
		users = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	q, args := p.appendTo(`SELECT `+capabilityGrantCols+` FROM capability_grants
		WHERE subject_type = 'all'
		   OR (subject_type = 'user'  AND subject = ANY($1::text[]))
		   OR (subject_type = 'group' AND subject = ANY($2::text[]))
		   OR (subject_type = 'user_type' AND subject = $3)
		ORDER BY capability, subject_type, subject, value`, []any{users, groups, userType})
	return collect(ctx, s.Pool, "list", "capability grants for subject", q, args, scanCapabilityGrant)
}

// ListApprovalsPage returns approvals filtered by state (empty = all) in
// reverse request order, bounded by p. The all-state feed rides
// approvals_requested_at_idx; a single-state filter rides
// approvals_state_requested_at_idx, serving both WHERE and ORDER BY without a sort.
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

// QueryAuditEventsPage returns a run's audit events in seq (chronological)
// order, bounded by p. audit_events_run_seq_idx makes WHERE run_id + ORDER
// BY seq an indexed range scan with no sort; OFFSET pages forward without
// flipping to DESC, so the per-run trail stays ASC (docs/sdk.md's exit-code
// contract) and a caller pages to the newest events with ?offset=.
func (s PG) QueryAuditEventsPage(ctx context.Context, runID uuid.UUID, p Page) ([]types.AuditEvent, error) {
	q, args := p.appendTo(`
		SELECT `+auditCols+`
		FROM audit_events WHERE run_id=$1 ORDER BY seq ASC`, []any{runID})
	return collect(ctx, s.Pool, "query", "audit events", q, args, scanAuditEvent)
}

// QueryRecentAuditEventsPage returns the newest-first global audit feed,
// bounded by p. seq is the audit_events PRIMARY KEY, so ORDER BY seq DESC +
// LIMIT is an index-scan-backward with no added index.
func (s PG) QueryRecentAuditEventsPage(ctx context.Context, p Page) ([]types.AuditEvent, error) {
	q, args := p.appendTo(`
		SELECT `+auditCols+`
		FROM audit_events ORDER BY seq DESC`, nil)
	return collect(ctx, s.Pool, "query", "recent audit events", q, args, scanAuditEvent)
}

// AWSSSOSpentTokenStore persists the AWS SSO refresh-token "spent" mark
// (internal/api/awssso_refresh.go's ssoRefreshSpent map) so it survives a
// daemon restart, through Store/PG rather than the secret/blob store since
// the mark is written precisely because that blob store failed.
type AWSSSOSpentTokenStore interface {
	// MarkAWSSSOTokenSpent upserts one spent-token row, idempotent: re-marking
	// an already-spent fingerprint is a no-op, so row age stays "since first
	// spent" for the prune below.
	MarkAWSSSOTokenSpent(ctx context.Context, fingerprint, owner string, markedAt time.Time) error
	// AWSSSOTokenSpent reports whether fingerprint is already known dead; its
	// one caller memoizes the answer, so a miss costs at most one query per
	// fingerprint per process lifetime.
	AWSSSOTokenSpent(ctx context.Context, fingerprint string) (bool, error)
	// PruneAWSSSOSpentTokens deletes rows marked before cutoff, reporting the
	// count removed. Called from the reaper's existing per-tick advisory lock.
	PruneAWSSSOSpentTokens(ctx context.Context, cutoff time.Time) (int, error)
}

var _ AWSSSOSpentTokenStore = PG{}

// MarkAWSSSOTokenSpent upserts idempotently: ON CONFLICT DO NOTHING keeps
// row age "since first spent" even on a second failed persist or invalid_grant.
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

// PruneAWSSSOSpentTokens deletes spent-token rows older than cutoff
// (aws_sso_spent_tokens_marked_at_idx backs the WHERE clause).
func (s PG) PruneAWSSSOSpentTokens(ctx context.Context, cutoff time.Time) (int, error) {
	const q = `DELETE FROM aws_sso_spent_tokens WHERE marked_at < $1`
	tag, err := s.Pool.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune aws sso spent tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
