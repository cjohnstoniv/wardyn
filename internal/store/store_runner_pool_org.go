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

// OrgRunnerPoolStore keeps an organization's own registered runners (owner_kind org, migration 0180) in
// its organization pools. A person's runner never joins an organization pool, and an org runner never a
// user pool (AddOwnRunnerToPool compares a non-empty owner).
type OrgRunnerPoolStore interface {
	AddOrgRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, orgURLSHA256 string) (m types.RunnerPoolMember, added bool, revision int64, err error)
	RemoveOrgRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID) (removed bool, revision int64, err error)
}

var _ OrgRunnerPoolStore = PG{}

// ErrRunnerInOtherPool: the org runner is already a member of another live pool. An org runner belongs
// to one organization pool, so who may use it is that pool's use policy and no second pool widens it.
var ErrRunnerInOtherPool = errors.New("store: the runner is already in another pool")

// AddOrgRunnerToPool adds a claimed org runner bound to this organisation's URL to a live organization
// pool, decided in the insert itself under the runner's and the pool's row locks. A user pool is
// ErrRunnerPoolScope; a runner in another live pool is ErrRunnerInOtherPool; ErrNotFound covers every
// other case alike (unknown, a person's, unclaimed, revoked, another organisation's).
func (s PG) AddOrgRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, orgURLSHA256 string) (types.RunnerPoolMember, bool, int64, error) {
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
		var other bool
		if err := q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM runner_pool_members m JOIN runner_pools p ON p.id = m.pool_id
			WHERE m.runner_id = $1 AND m.pool_id <> $2 AND p.state <> 'deleted')`, runnerID, id).Scan(&other); err != nil {
			return err
		}
		if other {
			return ErrRunnerInOtherPool
		}
		m.RunnerID = &runnerID
		err = q.QueryRow(ctx, `
			INSERT INTO runner_pool_members (pool_id, runner_id)
			SELECT p.id, r.id FROM runner_pools p, runners r
			WHERE p.id = $1 AND r.id = $2 AND p.scope = 'organization' AND r.owner_kind = 'org' AND r.owner = ''
			  AND r.state = 'claimed' AND r.org_url_sha256 = $3
			ON CONFLICT DO NOTHING RETURNING pool_id, kind, added_at`, id, runnerID, orgURLSHA256).Scan(&m.PoolID, &m.Kind, &m.AddedAt)
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
			SELECT m.pool_id, m.kind, m.added_at FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
			WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.owner_kind = 'org'`, id, runnerID).Scan(&m.PoolID, &m.Kind, &m.AddedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return m, added, rev, err
}

// RemoveOrgRunnerFromPool removes an org runner (in any state, so a revoked one can be tidied away) from
// a pool; removed is false when it was not a member.
func (s PG) RemoveOrgRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID) (bool, int64, error) {
	return s.removeMember(ctx, id, `
		DELETE FROM runner_pool_members m USING runners r
		WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.id = m.runner_id AND r.owner_kind = 'org'`, runnerID)
}
