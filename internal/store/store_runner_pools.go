// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The runner pool catalogue (migration 0192): pools, their members, the use policy of a
// remote_provided pool and the organisation's defaults. Every write that changes a pool, its members
// or its use policy holds the pool row and raises its revision by one in the same transaction, so a
// run admitted against a revision can tell that the pool moved. Whether a caller may use a pool is
// the API's rule, never this file's.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerPoolStore is the optional store capability behind the runner pool routes: a store without it
// answers them 501, as RunnerStore does.
type RunnerPoolStore interface {
	ListRunnerPoolCatalogue(ctx context.Context, owner string) ([]RunnerPoolCatalogueRow, error)
	GetRunnerPool(ctx context.Context, id uuid.UUID) (types.RunnerPool, error)
	CreateRunnerPool(ctx context.Context, p types.RunnerPool, by string) (types.RunnerPool, error)
	UpdateRunnerPool(ctx context.Context, id uuid.UUID, revision int64, name *string, state *types.RunnerPoolState) (types.RunnerPool, error)
	DeleteRunnerPool(ctx context.Context, id uuid.UUID) (types.RunnerPool, error)

	AddRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string) (types.RunnerPoolMember, bool, error)
	RemoveRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string) (bool, error)
	AddOwnRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, owner, orgURLSHA256 string) (types.RunnerPoolMember, bool, error)
	RemoveOwnRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID, owner string) (bool, error)

	PutRunnerPoolUsePolicy(ctx context.Context, id uuid.UUID, subjects []types.RunnerPoolSubject, by string) (types.RunnerPoolUsePolicy, error)
	DeleteRunnerPoolUsePolicy(ctx context.Context, id uuid.UUID) error

	GetRunnerPoolOrgDefaults(ctx context.Context) (types.RunnerPoolDefaults, error)
	PutRunnerPoolOrgDefaults(ctx context.Context, d types.RunnerPoolDefaults, by string) error
	BootstrapRunnerPools(ctx context.Context, executors []string, name string) (*types.RunnerPool, error)
}

var _ RunnerPoolStore = PG{}

var (
	// ErrRunnerPoolStale: the caller's revision is not the pool's.
	ErrRunnerPoolStale = errors.New("store: runner pool revision is stale")
	// ErrRunnerPoolLimit: the catalogue is at RunnerPoolsMax.
	ErrRunnerPoolLimit = errors.New("store: too many runner pools")
)

// RunnerPoolsMax bounds the live catalogue, so every catalogue read is bounded.
const RunnerPoolsMax = 200

// RunnerPoolCatalogueRow is one live pool with what the API needs to decide who may use it and
// whether it can take a run right now: its use policy, its executor members and how many of the
// owner's own claimed runners it holds.
type RunnerPoolCatalogueRow struct {
	Pool        types.RunnerPool
	Policy      *types.RunnerPoolUsePolicy
	ExecutorIDs []string
	OwnRunners  int
}

const runnerPoolCols = `id, name, hosting_type, state, revision, created_at, updated_at`

func scanRunnerPool(row pgx.Row) (types.RunnerPool, error) {
	var p types.RunnerPool
	var hosting, state string
	err := row.Scan(&p.ID, &p.Name, &hosting, &state, &p.Revision, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPool{}, ErrNotFound
	}
	if err != nil {
		return types.RunnerPool{}, fmt.Errorf("store: scan runner pool: %w", err)
	}
	p.HostingType, p.State = types.RunnerPoolHosting(hosting), types.RunnerPoolState(state)
	return p, nil
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// getRunnerPoolQ reads one pool, a deleted one included, and locks the row when forUpdate.
func getRunnerPoolQ(ctx context.Context, q Querier, id uuid.UUID, forUpdate bool) (types.RunnerPool, error) {
	return scanRunnerPool(q.QueryRow(ctx, `SELECT `+runnerPoolCols+` FROM runner_pools WHERE id = $1`+lockSuffix(forUpdate), id))
}

// livePoolForUpdate locks a pool that is not deleted: ErrNotFound for an unknown or deleted one.
func livePoolForUpdate(ctx context.Context, q Querier, id uuid.UUID) (types.RunnerPool, error) {
	p, err := getRunnerPoolQ(ctx, q, id, true)
	if err == nil && p.State == types.RunnerPoolDeleted {
		return types.RunnerPool{}, ErrNotFound
	}
	return p, err
}

func bumpRunnerPoolQ(ctx context.Context, q Querier, id uuid.UUID) (int64, error) {
	var rev int64
	err := q.QueryRow(ctx, `UPDATE runner_pools SET revision = revision + 1, updated_at = now() WHERE id = $1 RETURNING revision`, id).Scan(&rev)
	return rev, err
}

func (s PG) GetRunnerPool(ctx context.Context, id uuid.UUID) (types.RunnerPool, error) {
	return getRunnerPoolQ(ctx, s.Pool, id, false)
}

// ListRunnerPoolCatalogue returns every live pool, by name, with its use policy, executor members and
// the count of owner's own claimed runners it holds.
func (s PG) ListRunnerPoolCatalogue(ctx context.Context, owner string) ([]RunnerPoolCatalogueRow, error) {
	const q = `
		SELECT p.id, p.name, p.hosting_type, p.state, p.revision, p.created_at, p.updated_at,
		       u.subjects, u.revision, u.updated_by, u.updated_at,
		       COALESCE((SELECT array_agg(m.executor_id ORDER BY m.executor_id) FROM runner_pool_members m
		                 WHERE m.pool_id = p.id AND m.executor_id IS NOT NULL), '{}'),
		       (SELECT count(*) FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
		         WHERE m.pool_id = p.id AND r.owner = $1 AND r.state = 'claimed')
		FROM runner_pools p LEFT JOIN runner_pool_use_policies u ON u.pool_id = p.id
		WHERE p.state <> 'deleted' ORDER BY lower(p.name), p.id LIMIT $2`
	return collect(ctx, s.Pool, "list", "runner pools", q, []any{owner, RunnerPoolsMax}, func(row pgx.Row) (RunnerPoolCatalogueRow, error) {
		var r RunnerPoolCatalogueRow
		var hosting, state string
		var subjects []byte
		var polRev *int64
		var polBy *string
		var polAt *time.Time
		if err := row.Scan(&r.Pool.ID, &r.Pool.Name, &hosting, &state, &r.Pool.Revision, &r.Pool.CreatedAt, &r.Pool.UpdatedAt,
			&subjects, &polRev, &polBy, &polAt, &r.ExecutorIDs, &r.OwnRunners); err != nil {
			return r, fmt.Errorf("store: scan runner pool catalogue: %w", err)
		}
		r.Pool.HostingType, r.Pool.State = types.RunnerPoolHosting(hosting), types.RunnerPoolState(state)
		if subjects != nil {
			pol := types.RunnerPoolUsePolicy{PoolID: r.Pool.ID, Revision: *polRev, UpdatedBy: *polBy, UpdatedAt: *polAt}
			if err := json.Unmarshal(subjects, &pol.Subjects); err != nil {
				return r, fmt.Errorf("store: unmarshal runner pool use policy: %w", err)
			}
			r.Policy = &pol
		}
		return r, nil
	})
}

// CreateRunnerPool inserts an active pool at revision 1. ErrConflict: a live pool has the name.
// ErrRunnerPoolLimit: the catalogue is full.
func (s PG) CreateRunnerPool(ctx context.Context, p types.RunnerPool, by string) (types.RunnerPool, error) {
	var out types.RunnerPool
	err := s.inTx(ctx, func(q Querier) error {
		// One lock for the count and the insert, so two creates cannot both pass the cap.
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('runner_pools.create'))`); err != nil {
			return err
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM runner_pools WHERE state <> 'deleted'`).Scan(&n); err != nil {
			return err
		}
		if n >= RunnerPoolsMax {
			return ErrRunnerPoolLimit
		}
		return insertRunnerPoolQ(ctx, q, p, by, &out)
	})
	return out, err
}

func insertRunnerPoolQ(ctx context.Context, q Querier, p types.RunnerPool, by string, out *types.RunnerPool) error {
	var err error
	*out, err = scanRunnerPool(q.QueryRow(ctx, `
		INSERT INTO runner_pools (id, name, hosting_type, created_by) VALUES ($1,$2,$3,$4) RETURNING `+runnerPoolCols,
		p.ID, p.Name, string(p.HostingType), by))
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// UpdateRunnerPool renames or switches a pool, only at the revision the caller read: ErrRunnerPoolStale
// otherwise, ErrNotFound for an unknown or deleted pool, ErrConflict when the name is taken. A change
// that moves nothing writes nothing and keeps the revision.
func (s PG) UpdateRunnerPool(ctx context.Context, id uuid.UUID, revision int64, name *string, state *types.RunnerPoolState) (types.RunnerPool, error) {
	var out types.RunnerPool
	err := s.inTx(ctx, func(q Querier) error {
		cur, err := livePoolForUpdate(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return ErrRunnerPoolStale
		}
		if (name == nil || *name == cur.Name) && (state == nil || *state == cur.State) {
			out = cur
			return nil
		}
		out, err = scanRunnerPool(q.QueryRow(ctx, `
			UPDATE runner_pools SET name = COALESCE($2, name), state = COALESCE($3, state),
			       revision = revision + 1, updated_at = now()
			WHERE id = $1 RETURNING `+runnerPoolCols, id, name, stateText(state)))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	return out, err
}

// stateText is a state as the nullable text COALESCE reads.
func stateText(s *types.RunnerPoolState) *string {
	if s == nil {
		return nil
	}
	v := string(*s)
	return &v
}

// DeleteRunnerPool tombstones a live pool and drops its members and use policy: ErrNotFound for an
// unknown or already deleted one. The row stays, so a run bound to it keeps its history.
func (s PG) DeleteRunnerPool(ctx context.Context, id uuid.UUID) (types.RunnerPool, error) {
	var out types.RunnerPool
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := livePoolForUpdate(ctx, q, id); err != nil {
			return err
		}
		for _, stmt := range []string{
			`DELETE FROM runner_pool_members WHERE pool_id = $1`,
			`DELETE FROM runner_pool_use_policies WHERE pool_id = $1`,
		} {
			if _, err := q.Exec(ctx, stmt, id); err != nil {
				return err
			}
		}
		var err error
		out, err = scanRunnerPool(q.QueryRow(ctx, `
			UPDATE runner_pools SET state = 'deleted', revision = revision + 1, updated_at = now()
			WHERE id = $1 RETURNING `+runnerPoolCols, id))
		return err
	})
	return out, err
}

// AddRunnerPoolExecutor adds a configured executor to a live remote_provided pool; added is false when
// it was already a member (no revision change). ErrNotFound: no such live pool of that hosting type.
func (s PG) AddRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string) (types.RunnerPoolMember, bool, error) {
	var m types.RunnerPoolMember
	var added bool
	err := s.inTx(ctx, func(q Querier) error {
		p, err := livePoolForUpdate(ctx, q, id)
		if err != nil || p.HostingType != types.RunnerPoolRemoteProvided {
			return errOrNotFound(err)
		}
		tag, err := q.Exec(ctx, `INSERT INTO runner_pool_members (pool_id, executor_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, executor)
		if err != nil {
			return err
		}
		added = tag.RowsAffected() == 1
		if added {
			if _, err := bumpRunnerPoolQ(ctx, q, id); err != nil {
				return err
			}
		}
		return q.QueryRow(ctx, `SELECT pool_id, executor_id, added_at FROM runner_pool_members WHERE pool_id = $1 AND executor_id = $2`, id, executor).
			Scan(&m.PoolID, &m.ExecutorID, &m.AddedAt)
	})
	return m, added, err
}

// errOrNotFound is err, or ErrNotFound when err is nil: a pool of the wrong hosting type is not there.
func errOrNotFound(err error) error {
	if err != nil {
		return err
	}
	return ErrNotFound
}

// RemoveRunnerPoolExecutor removes an executor member; removed is false when it was not one.
func (s PG) RemoveRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string) (bool, error) {
	var removed bool
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := livePoolForUpdate(ctx, q, id); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `DELETE FROM runner_pool_members WHERE pool_id = $1 AND executor_id = $2`, id, executor)
		if err != nil {
			return err
		}
		if removed = tag.RowsAffected() == 1; removed {
			_, err = bumpRunnerPoolQ(ctx, q, id)
		}
		return err
	})
	return removed, err
}

// AddOwnRunnerToPool adds a runner to a live self_hosted pool only when owner owns it, it is claimed
// and bound to this organisation's URL, all decided in the insert itself so a revocation or a transfer
// cannot slip between a check and the write. ErrNotFound covers every other case alike: an unknown
// pool or runner, a pool of the other hosting type, and a runner that is not the owner's.
func (s PG) AddOwnRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, owner, orgURLSHA256 string) (types.RunnerPoolMember, bool, error) {
	var m types.RunnerPoolMember
	var added bool
	err := s.inTx(ctx, func(q Querier) error {
		p, err := livePoolForUpdate(ctx, q, id)
		if err != nil || p.HostingType != types.RunnerPoolSelfHosted {
			return errOrNotFound(err)
		}
		const own = `FROM runners WHERE id = $2 AND owner = $3 AND state = 'claimed' AND org_url_sha256 = $4`
		tag, err := q.Exec(ctx, `INSERT INTO runner_pool_members (pool_id, runner_id) SELECT $1, id `+own+` ON CONFLICT DO NOTHING`, id, runnerID, owner, orgURLSHA256)
		if err != nil {
			return err
		}
		if added = tag.RowsAffected() == 1; added {
			if _, err := bumpRunnerPoolQ(ctx, q, id); err != nil {
				return err
			}
		}
		m.RunnerID = &runnerID
		err = q.QueryRow(ctx, `
			SELECT m.pool_id, m.added_at FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
			WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.owner = $3`, id, runnerID, owner).Scan(&m.PoolID, &m.AddedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return m, added, err
}

// RemoveOwnRunnerFromPool removes owner's own runner (in any state, so a revoked one can be tidied
// away) from a pool; removed is false when it was not a member or not owner's.
func (s PG) RemoveOwnRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID, owner string) (bool, error) {
	var removed bool
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := livePoolForUpdate(ctx, q, id); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			DELETE FROM runner_pool_members m USING runners r
			WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.id = m.runner_id AND r.owner = $3`, id, runnerID, owner)
		if err != nil {
			return err
		}
		if removed = tag.RowsAffected() == 1; removed {
			_, err = bumpRunnerPoolQ(ctx, q, id)
		}
		return err
	})
	return removed, err
}

// GetRunnerPoolUsePolicyQ reads one pool's use policy on q, ErrNotFound when it has none.
func GetRunnerPoolUsePolicyQ(ctx context.Context, q Querier, id uuid.UUID) (types.RunnerPoolUsePolicy, error) {
	pol := types.RunnerPoolUsePolicy{PoolID: id}
	var subjects []byte
	err := q.QueryRow(ctx, `SELECT subjects, revision, updated_by, updated_at FROM runner_pool_use_policies WHERE pool_id = $1`, id).
		Scan(&subjects, &pol.Revision, &pol.UpdatedBy, &pol.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pol, ErrNotFound
	}
	if err != nil {
		return pol, fmt.Errorf("store: read runner pool use policy: %w", err)
	}
	return pol, json.Unmarshal(subjects, &pol.Subjects)
}

// PutRunnerPoolUsePolicy sets a live remote_provided pool's use policy and raises the pool's revision.
// It takes the governance target lock an approval of a held change takes too.
func (s PG) PutRunnerPoolUsePolicy(ctx context.Context, id uuid.UUID, subjects []types.RunnerPoolSubject, by string) (types.RunnerPoolUsePolicy, error) {
	var out types.RunnerPoolUsePolicy
	err := s.inTx(ctx, func(q Querier) error {
		if err := LockGovernanceTarget(ctx, q, types.GovernanceTargetRunnerPoolUsePolicy, id.String()); err != nil {
			return err
		}
		var err error
		out, err = PutRunnerPoolUsePolicyQ(ctx, q, id, subjects, by)
		return err
	})
	return out, err
}

// PutRunnerPoolUsePolicyQ is PutRunnerPoolUsePolicy's statement on q (a pgx.Tx, the pool row locked
// here). ErrNotFound: no such live remote_provided pool.
func PutRunnerPoolUsePolicyQ(ctx context.Context, q Querier, id uuid.UUID, subjects []types.RunnerPoolSubject, by string) (types.RunnerPoolUsePolicy, error) {
	p, err := livePoolForUpdate(ctx, q, id)
	if err != nil || p.HostingType != types.RunnerPoolRemoteProvided {
		return types.RunnerPoolUsePolicy{}, errOrNotFound(err)
	}
	raw, err := json.Marshal(subjects)
	if err != nil {
		return types.RunnerPoolUsePolicy{}, err
	}
	rev, err := bumpRunnerPoolQ(ctx, q, id)
	if err != nil {
		return types.RunnerPoolUsePolicy{}, err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO runner_pool_use_policies (pool_id, subjects, revision, updated_by) VALUES ($1,$2,$3,$4)
		ON CONFLICT (pool_id) DO UPDATE SET subjects = EXCLUDED.subjects, revision = EXCLUDED.revision,
		       updated_by = EXCLUDED.updated_by, updated_at = now()`, id, raw, rev, by); err != nil {
		return types.RunnerPoolUsePolicy{}, err
	}
	return GetRunnerPoolUsePolicyQ(ctx, q, id)
}

// DeleteRunnerPoolUsePolicy removes a pool's use policy, which widens the pool back to everyone who may
// launch remote runs, and raises the pool's revision. ErrNotFound: the pool has none (or is gone).
func (s PG) DeleteRunnerPoolUsePolicy(ctx context.Context, id uuid.UUID) error {
	return s.inTx(ctx, func(q Querier) error {
		if err := LockGovernanceTarget(ctx, q, types.GovernanceTargetRunnerPoolUsePolicy, id.String()); err != nil {
			return err
		}
		return DeleteRunnerPoolUsePolicyQ(ctx, q, id)
	})
}

// DeleteRunnerPoolUsePolicyQ is DeleteRunnerPoolUsePolicy's statement on q.
func DeleteRunnerPoolUsePolicyQ(ctx context.Context, q Querier, id uuid.UUID) error {
	if _, err := livePoolForUpdate(ctx, q, id); err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `DELETE FROM runner_pool_use_policies WHERE pool_id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = bumpRunnerPoolQ(ctx, q, id)
	return err
}

// GetRunnerPoolOrgDefaults reads the organisation's defaults; the zero value when none were written.
func (s PG) GetRunnerPoolOrgDefaults(ctx context.Context) (types.RunnerPoolDefaults, error) {
	var d types.RunnerPoolDefaults
	var hosting string
	err := s.Pool.QueryRow(ctx, `SELECT preferred_hosting, remote_provided, self_hosted FROM runner_pool_org_defaults`).Scan(&hosting, &d.RemoteProvided, &d.SelfHosted)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPoolDefaults{}, nil
	}
	d.PreferredHosting = types.RunnerPoolHosting(hosting)
	return d, err
}

// PutRunnerPoolOrgDefaults replaces the organisation's defaults. The caller has checked the pools; a
// default only names one, and the resolver refuses a name that has since gone or been switched off.
func (s PG) PutRunnerPoolOrgDefaults(ctx context.Context, d types.RunnerPoolDefaults, by string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO runner_pool_org_defaults (preferred_hosting, remote_provided, self_hosted, updated_by) VALUES ($1,$2,$3,$4)
		ON CONFLICT (singleton) DO UPDATE SET preferred_hosting = EXCLUDED.preferred_hosting,
		       remote_provided = EXCLUDED.remote_provided, self_hosted = EXCLUDED.self_hosted,
		       updated_by = EXCLUDED.updated_by, updated_at = now()`,
		string(d.PreferredHosting), d.RemoteProvided, d.SelfHosted, by)
	return err
}

// BootstrapRunnerPools is the one-time upgrade step: when this deployment has configured executors and
// no pool has ever existed (a deleted one still counts, so an administrator's deletion sticks), it
// creates one remote_provided pool named name holding exactly those executors and makes it the
// organisation's remote default, so existing runs keep the placement they have today. It grants
// nothing a person lacked (no use policy, and no self-hosted pool is ever made). With no executors or
// a catalogue that has existed it does nothing. Returns the pool it created, nil otherwise.
func (s PG) BootstrapRunnerPools(ctx context.Context, executors []string, name string) (*types.RunnerPool, error) {
	if len(executors) == 0 {
		return nil, nil
	}
	var created *types.RunnerPool
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('runner_pools.create'))`); err != nil {
			return err
		}
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pools)`).Scan(&exists); err != nil || exists {
			return err
		}
		var p types.RunnerPool
		if err := insertRunnerPoolQ(ctx, q, types.RunnerPool{ID: uuid.New(), Name: name, HostingType: types.RunnerPoolRemoteProvided}, "system", &p); err != nil {
			return err
		}
		for _, e := range executors {
			if _, err := q.Exec(ctx, `INSERT INTO runner_pool_members (pool_id, executor_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, p.ID, e); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO runner_pool_org_defaults (remote_provided, updated_by) VALUES ($1, 'system')
			ON CONFLICT (singleton) DO UPDATE SET remote_provided = EXCLUDED.remote_provided, updated_by = 'system', updated_at = now()`, p.ID); err != nil {
			return err
		}
		created = &p
		return nil
	})
	return created, err
}
