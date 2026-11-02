// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// OrgRunnerPoolStore keeps an organization's own registered runners (owner_kind org, migration 0181) in
// its organization pools. A person's runner never joins an organization pool, and an org runner never a
// user pool (AddOwnRunnerToPool compares a non-empty owner).
type OrgRunnerPoolStore interface {
	AddOrgRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, orgURLSHA256, runsNamespace string) (m types.RunnerPoolMember, added bool, revision int64, err error)
	RemoveOrgRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID, refuseGoverned bool) (removed bool, revision int64, err error)
}

var _ OrgRunnerPoolStore = PG{}

// ErrRunnerInOtherPool: the org runner is already a member of another live pool in a way the new
// membership overlaps. An org runner serves each of its runs namespaces for one organization pool, so who
// may use it there is that pool's use policy and no second pool widens it; the runner's default namespace
// (an empty runs namespace) overlaps every other.
var ErrRunnerInOtherPool = errors.New("store: the runner is already in another pool")

// ErrRunnerNamespaceTaken: another live pool already has the org runner serving this runs namespace.
var ErrRunnerNamespaceTaken = errors.New("store: the runner already serves the namespace for another pool")

// ErrRunnerNamespaceFixed: the org runner is already in this pool in a different runs namespace, which
// is set when the runner is added and changes only by removing it and adding it again.
var ErrRunnerNamespaceFixed = errors.New("store: the runner is in the pool in another namespace")

// AddOrgRunnerToPool adds a claimed org runner bound to this organisation's URL to a live organization
// pool, serving runsNamespace ("" for the runner's default), decided in the insert itself under the
// runner's and the pool's row locks. A user pool is ErrRunnerPoolScope; another live membership of the
// runner with the same namespace, or with an empty one on either side, is ErrRunnerInOtherPool (or
// ErrRunnerNamespaceTaken for the same named namespace); a runner already in this pool in another
// namespace is ErrRunnerNamespaceFixed with its member returned; ErrNotFound covers every other case
// alike (unknown, a person's, unclaimed, revoked, another organisation's).
func (s PG) AddOrgRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, orgURLSHA256, runsNamespace string) (types.RunnerPoolMember, bool, int64, error) {
	var m types.RunnerPoolMember
	var added bool
	var rev int64
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `SELECT 1 FROM runners WHERE id = $1 FOR UPDATE`, runnerID); err != nil {
			return err
		}
		p, err := livePoolForUpdate(ctx, q, id)
		if err != nil {
			return err
		}
		if p.Scope != types.RunnerPoolOrganization {
			return ErrRunnerPoolScope
		}
		// A runner already in this pool keeps the namespace it was added with: a re-add answers that member,
		// or ErrRunnerNamespaceFixed for another namespace, before any other pool's membership is weighed.
		m.RunnerID = &runnerID
		err = q.QueryRow(ctx, `
			SELECT m.pool_id, m.kind, m.runs_namespace, m.added_at FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
			WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.owner_kind = 'org'`, id, runnerID).Scan(&m.PoolID, &m.Kind, &m.RunsNamespace, &m.AddedAt)
		switch {
		case err == nil && m.RunsNamespace != runsNamespace:
			return ErrRunnerNamespaceFixed
		case err == nil:
			rev = p.Revision
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// The runner's other live memberships that overlap this one, the exact namespace first.
		var clash string
		err = q.QueryRow(ctx, `
			SELECT m.runs_namespace FROM runner_pool_members m JOIN runner_pools p ON p.id = m.pool_id
			WHERE m.runner_id = $1 AND m.pool_id <> $2 AND p.state <> 'deleted'
			  AND ($3 = '' OR m.runs_namespace = '' OR m.runs_namespace = $3)
			ORDER BY (m.runs_namespace = $3) DESC LIMIT 1`, runnerID, id, runsNamespace).Scan(&clash)
		switch {
		case err == nil && runsNamespace != "" && clash == runsNamespace:
			return ErrRunnerNamespaceTaken
		case err == nil:
			return ErrRunnerInOtherPool
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		m.RunsNamespace = runsNamespace
		err = q.QueryRow(ctx, `
			INSERT INTO runner_pool_members (pool_id, runner_id, runs_namespace)
			SELECT p.id, r.id, $4::text FROM runner_pools p, runners r
			WHERE p.id = $1 AND r.id = $2 AND p.scope = 'organization' AND r.owner_kind = 'org' AND r.owner = ''
			  AND r.state = 'claimed' AND r.org_url_sha256 = $3
			ON CONFLICT DO NOTHING RETURNING pool_id, kind, added_at`, id, runnerID, orgURLSHA256, runsNamespace).Scan(&m.PoolID, &m.Kind, &m.AddedAt)
		if err == nil {
			added = true
			rev, err = bumpRunnerPoolQ(ctx, q, id)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		rev = p.Revision
		err = q.QueryRow(ctx, `
			SELECT m.pool_id, m.kind, m.runs_namespace, m.added_at FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
			WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.owner_kind = 'org'`, id, runnerID).Scan(&m.PoolID, &m.Kind, &m.RunsNamespace, &m.AddedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err == nil && m.RunsNamespace != runsNamespace {
			return ErrRunnerNamespaceFixed
		}
		return err
	})
	return m, added, rev, err
}

// RemoveOrgRunnerFromPool removes an org runner (in any state, so a revoked one can be tidied away) from
// a pool; removed is false when it was not a member. The caller decided on a catalogue read taken before
// the locks whether the removal is held; refuseGoverned says it decided "not held", and
// ErrRunnerPoolGoverned answers a use policy that was approved in between.
func (s PG) RemoveOrgRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID, refuseGoverned bool) (removed bool, rev int64, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		if err := LockGovernanceTarget(ctx, q, types.GovernanceTargetRunnerPoolUsePolicy, id.String()); err != nil {
			return err
		}
		if refuseGoverned {
			if err := LockLivePoolQ(ctx, q, id); err != nil {
				return err
			}
			var governed bool
			if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pool_use_policies WHERE pool_id = $1)
				AND EXISTS (SELECT 1 FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
				            WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.owner_kind = 'org')`, id, runnerID).Scan(&governed); err != nil {
				return err
			}
			if governed {
				return ErrRunnerPoolGoverned
			}
		}
		removed, rev, err = RemoveOrgRunnerFromPoolQ(ctx, q, id, runnerID)
		return err
	})
	return removed, rev, err
}

// RemoveOrgRunnerFromPoolQ is RemoveOrgRunnerFromPool's statement on q (a pgx.Tx), which a held removal
// applies through inside the decision transaction.
func RemoveOrgRunnerFromPoolQ(ctx context.Context, q Querier, id, runnerID uuid.UUID) (bool, int64, error) {
	return removeMemberQ(ctx, q, id, `
		DELETE FROM runner_pool_members m USING runners r
		WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.id = m.runner_id AND r.owner_kind = 'org'`, runnerID)
}

// OrgRunnerPoolRunnerIDsQ lists a pool's organization runners' ids, sorted as text, never nil.
func OrgRunnerPoolRunnerIDsQ(ctx context.Context, q Querier, id uuid.UUID) ([]string, error) {
	return collect(ctx, q, "list", "runner pool organization runners", `
		SELECT m.runner_id::text FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
		WHERE m.pool_id = $1 AND r.owner_kind = 'org' ORDER BY m.runner_id::text`,
		[]any{id}, func(r pgx.Row) (string, error) {
			var v string
			return v, r.Scan(&v)
		})
}

// ListOrgRunnerPoolRunners is an organization pool's organization runners, newest first, in any state, so
// a revoked one can be seen and tidied away with RemoveOrgRunnerFromPool.
func (s PG) ListOrgRunnerPoolRunners(ctx context.Context, poolID uuid.UUID) ([]types.Runner, error) {
	return collect(ctx, s.Pool, "list", "organization pool runners", `SELECT `+prefixedRunnerCols+`
		FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
		WHERE m.pool_id = $1 AND r.owner_kind = 'org' ORDER BY r.created_at DESC, r.id`, []any{poolID}, scanRunner)
}
