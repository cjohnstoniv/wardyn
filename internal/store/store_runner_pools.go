// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The runner pool catalogue (migrations 0152, 0168, 0173): pools, their members, the use policy of an
// organization pool and the organisation's default. Every write that changes a pool, its members
// or its use policy holds the pool row and raises its revision by one in the same transaction, so a
// run admitted against a revision can tell that the pool moved. Whether a caller may use a pool is
// the API's rule, never this file's.
package store

import (
	"bytes"
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
	// ListRunnerPoolCatalogue is the live organization pools and owner's own user pools; with all,
	// every person's user pools too, for oversight.
	ListRunnerPoolCatalogue(ctx context.Context, owner string, all bool) ([]RunnerPoolCatalogueRow, error)
	GetRunnerPool(ctx context.Context, id uuid.UUID) (types.RunnerPool, error)
	// ListRunnerPoolMembers is every member of a pool: only the admission fold reads it, and only through
	// runnerpool.OwnRunnerCandidates.
	ListRunnerPoolMembers(ctx context.Context, id uuid.UUID) ([]types.RunnerPoolMember, error)
	CreateRunnerPool(ctx context.Context, p types.RunnerPool, by string) (types.RunnerPool, error)
	UpdateRunnerPool(ctx context.Context, id uuid.UUID, revision int64, name *string, state *types.RunnerPoolState, limits *types.RunnerPoolLimits, recording *types.RecordingRequirement) (types.RunnerPool, error)
	DeleteRunnerPool(ctx context.Context, id uuid.UUID) (RunnerPoolDeletion, error)
	DeleteRunnerPoolsByOwner(ctx context.Context, owner string) ([]RunnerPoolDeletion, error)

	AddRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string) (m types.RunnerPoolMember, added bool, revision int64, err error)
	RemoveRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string, refuseGoverned bool) (removed bool, revision int64, err error)
	AddOwnRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, owner, orgURLSHA256 string) (m types.RunnerPoolMember, added bool, revision int64, err error)
	RemoveOwnRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID, owner string) (removed bool, revision int64, err error)
	// AddRemoteRunnerToUserPool puts a remote-hosted runner in owner's own live user pool, only while
	// the runner's revision is still peopleRevision, the one whose people policy the caller checked.
	AddRemoteRunnerToUserPool(ctx context.Context, id uuid.UUID, remoteRunner, owner string, peopleRevision int64) (m types.RunnerPoolMember, added bool, revision int64, err error)
	RemoveRemoteRunnerFromUserPool(ctx context.Context, id uuid.UUID, remoteRunner, owner string) (removed bool, revision int64, err error)

	PutRunnerPoolUsePolicy(ctx context.Context, id uuid.UUID, revision *int64, subjects []types.RunnerPoolSubject, by string) (types.RunnerPoolUsePolicy, error)
	DeleteRunnerPoolUsePolicy(ctx context.Context, id uuid.UUID) error

	GetRunnerPoolOrgDefaults(ctx context.Context) (types.RunnerPoolDefaults, error)
	PutRunnerPoolOrgDefaults(ctx context.Context, d types.RunnerPoolDefaults, by string) (before, after types.RunnerPoolDefaults, err error)
	BootstrapRunnerPools(ctx context.Context, executors []string, name string) (*types.RunnerPool, *types.RunnerPoolDefaults, error)
}

var _ RunnerPoolStore = PG{}

var (
	// ErrRunnerPoolStale: the caller's revision is not the pool's.
	ErrRunnerPoolStale = errors.New("store: runner pool revision is stale")
	// ErrRunnerPoolGoverned: a detach that was decided as direct met a use policy under the locks.
	ErrRunnerPoolGoverned = errors.New("store: runner pool now has a use policy, so the detach must be held")
	// ErrRunnerPoolLimit: the organisation is at RunnerPoolsMax pools, or the owner at RunnerPoolsPerOwnerMax.
	ErrRunnerPoolLimit = errors.New("store: too many runner pools")
	// ErrRunnerPoolScope: a self-hosted runner was offered to an organization pool, which takes
	// remote-hosted runners only.
	ErrRunnerPoolScope = errors.New("store: organization pools take remote-hosted runners only")
	// ErrRunnerPoolHasPolicy: a pool with a use policy is not deleted, because the delete would clear
	// the policy outside the second-human hold; clear the policy first.
	ErrRunnerPoolHasPolicy = errors.New("store: the runner pool still has a use policy")
)

// RunnerPoolsMax bounds the organisation's live pools and RunnerPoolsPerOwnerMax one person's, so a
// person's catalogue read is bounded and no person's pools can crowd the organisation's out of it.
// RunnerPoolsOversightMax bounds the oversight list, which lists the organisation's pools and then the
// caller's own before anyone else's, so neither can drop out of it. The read takes one more row, so the
// list can say that more exist.
const (
	RunnerPoolsMax          = 200
	RunnerPoolsPerOwnerMax  = 10
	RunnerPoolsOversightMax = 1000
)

// RunnerPoolCatalogueRow is one live pool with what the API needs to decide who may use it and
// whether it can take a run right now: its use policy, its remote-hosted members (ExecutorIDs, their
// ids), how many of the caller's own claimed runners it holds, and which of the caller's own runners
// (in any state) are its members.
type RunnerPoolCatalogueRow struct {
	Pool         types.RunnerPool
	Policy       *types.RunnerPoolUsePolicy
	ExecutorIDs  []string
	OwnRunners   int
	OwnRunnerIDs []uuid.UUID
}

// RunnerPoolDeletion is what a delete removed, for its audit row: the pool as it was, the executors and
// the count of runners it held, and the organisation defaults before and after when the delete cleared
// a slot that named it.
type RunnerPoolDeletion struct {
	Pool              types.RunnerPool
	ExecutorIDs       []string
	RunnerCount       int
	OrgDefaultsBefore *types.RunnerPoolDefaults
	OrgDefaultsAfter  *types.RunnerPoolDefaults
	// CancelledRuns are the runs that were waiting for the pool and were ended KILLED with it.
	CancelledRuns []uuid.UUID
}

const runnerPoolCols = `id, name, scope, COALESCE(owner, ''), state, revision, limits, created_at, updated_at, recording`

func scanRunnerPool(row pgx.Row) (types.RunnerPool, error) {
	var p types.RunnerPool
	var scope, state, recording string
	var limits []byte
	err := row.Scan(&p.ID, &p.Name, &scope, &p.Owner, &state, &p.Revision, &limits, &p.CreatedAt, &p.UpdatedAt, &recording)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPool{}, ErrNotFound
	}
	if err != nil {
		return types.RunnerPool{}, fmt.Errorf("store: scan runner pool: %w", err)
	}
	p.Scope, p.State, p.Recording = types.RunnerPoolScope(scope), types.RunnerPoolState(state), types.RecordingRequirement(recording)
	if p.Limits, err = unmarshalPoolLimits(limits); err != nil {
		return types.RunnerPool{}, err
	}
	return p, nil
}

// unmarshalPoolLimits reads the limits column: NULL is nil, a pool that adds no bound of its own.
func unmarshalPoolLimits(raw []byte) (*types.RunnerPoolLimits, error) {
	if raw == nil {
		return nil, nil
	}
	var l types.RunnerPoolLimits
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("store: unmarshal runner pool limits: %w", err)
	}
	return &l, nil
}

// marshalPoolLimits is the column value for limits: nil is NULL.
func marshalPoolLimits(l *types.RunnerPoolLimits) ([]byte, error) {
	if l == nil {
		return nil, nil
	}
	return json.Marshal(l)
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

func (s PG) ListRunnerPoolMembers(ctx context.Context, id uuid.UUID) ([]types.RunnerPoolMember, error) {
	return collect(ctx, s.Pool, "list", "runner pool members",
		`SELECT pool_id, kind, runner_id, COALESCE(remote_runner_id, ''), added_at FROM runner_pool_members WHERE pool_id = $1 ORDER BY runner_id, remote_runner_id`,
		[]any{id}, func(row pgx.Row) (types.RunnerPoolMember, error) {
			var m types.RunnerPoolMember
			if err := row.Scan(&m.PoolID, &m.Kind, &m.RunnerID, &m.RemoteRunnerID, &m.AddedAt); err != nil {
				return m, fmt.Errorf("store: scan runner pool member: %w", err)
			}
			return m, nil
		})
}

// ListRunnerPoolCatalogue returns the live organization pools and owner's own live user pools
// (with all, every person's: the organisation's first, then owner's own), by name, each with its use policy, remote-hosted
// members, the count of owner's own claimed runners it holds and the ids of owner's runners in it.
func (s PG) ListRunnerPoolCatalogue(ctx context.Context, owner string, all bool) ([]RunnerPoolCatalogueRow, error) {
	const q = `
		SELECT p.id, p.name, p.scope, COALESCE(p.owner, ''), p.state, p.revision, p.limits, p.created_at, p.updated_at, p.recording,
		       u.subjects, u.revision, u.updated_by, u.updated_at,
		       COALESCE((SELECT array_agg(m.remote_runner_id ORDER BY m.remote_runner_id) FROM runner_pool_members m
		                 WHERE m.pool_id = p.id AND m.remote_runner_id IS NOT NULL), '{}'),
		       (SELECT count(*) FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
		         WHERE m.pool_id = p.id AND r.owner = $1 AND r.state = 'claimed'),
		       COALESCE((SELECT array_agg(m.runner_id ORDER BY m.runner_id) FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
		                 WHERE m.pool_id = p.id AND r.owner = $1 AND $1 <> ''), '{}')
		FROM runner_pools p LEFT JOIN runner_pool_use_policies u ON u.pool_id = p.id
		WHERE p.state <> 'deleted' AND ($3 OR p.owner IS NULL OR p.owner = $1)
		ORDER BY CASE WHEN p.owner IS NULL THEN 0 WHEN p.owner = $1 THEN 1 ELSE 2 END, lower(p.name), p.id LIMIT $2`
	limit := RunnerPoolsMax + RunnerPoolsPerOwnerMax
	if all {
		limit = RunnerPoolsOversightMax + 1
	}
	return collect(ctx, s.Pool, "list", "runner pools", q, []any{owner, limit, all}, func(row pgx.Row) (RunnerPoolCatalogueRow, error) {
		var r RunnerPoolCatalogueRow
		var scope, state, recording string
		var subjects, limits []byte
		var polRev *int64
		var polBy *string
		var polAt *time.Time
		if err := row.Scan(&r.Pool.ID, &r.Pool.Name, &scope, &r.Pool.Owner, &state, &r.Pool.Revision, &limits, &r.Pool.CreatedAt, &r.Pool.UpdatedAt, &recording,
			&subjects, &polRev, &polBy, &polAt, &r.ExecutorIDs, &r.OwnRunners, &r.OwnRunnerIDs); err != nil {
			return r, fmt.Errorf("store: scan runner pool catalogue: %w", err)
		}
		r.Pool.Scope, r.Pool.State, r.Pool.Recording = types.RunnerPoolScope(scope), types.RunnerPoolState(state), types.RecordingRequirement(recording)
		var err error
		if r.Pool.Limits, err = unmarshalPoolLimits(limits); err != nil {
			return r, err
		}
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

// CreateRunnerPool inserts an active pool at revision 1, owned by p.Owner when it is set. ErrConflict:
// a live pool of the same owner (or of the organisation) has the name. ErrRunnerPoolLimit: the
// organisation, or the owner, is at its cap.
func (s PG) CreateRunnerPool(ctx context.Context, p types.RunnerPool, by string) (types.RunnerPool, error) {
	var out types.RunnerPool
	err := s.inTx(ctx, func(q Querier) error {
		// One lock for the count and the insert, so two creates cannot both pass the cap.
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('runner_pools.create'))`); err != nil {
			return err
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM runner_pools WHERE state <> 'deleted' AND owner IS NOT DISTINCT FROM NULLIF($1, '')`,
			p.Owner).Scan(&n); err != nil {
			return err
		}
		limit := RunnerPoolsMax
		if p.Owner != "" {
			limit = RunnerPoolsPerOwnerMax
		}
		if n >= limit {
			return ErrRunnerPoolLimit
		}
		return insertRunnerPoolQ(ctx, q, p, by, &out)
	})
	return out, err
}

func insertRunnerPoolQ(ctx context.Context, q Querier, p types.RunnerPool, by string, out *types.RunnerPool) error {
	limits, err := marshalPoolLimits(p.Limits)
	if err != nil {
		return err
	}
	*out, err = scanRunnerPool(q.QueryRow(ctx, `
		INSERT INTO runner_pools (id, name, scope, owner, created_by, limits, recording) VALUES ($1,$2,$3,NULLIF($4, ''),$5,$6,$7) RETURNING `+runnerPoolCols,
		p.ID, p.Name, string(p.Scope), p.Owner, by, limits, string(p.Recording)))
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// UpdateRunnerPool renames, switches, re-limits or sets the recording requirement of a pool, only at the
// revision the caller read: ErrRunnerPoolStale otherwise, ErrNotFound for an unknown or deleted pool,
// ErrConflict when the name is taken. A non-nil limits replaces the stored block whole; nil leaves it. A
// change that moves nothing, limits compared as canonical JSON, writes nothing and keeps the revision.
func (s PG) UpdateRunnerPool(ctx context.Context, id uuid.UUID, revision int64, name *string, state *types.RunnerPoolState, limits *types.RunnerPoolLimits, recording *types.RecordingRequirement) (types.RunnerPool, error) {
	var out types.RunnerPool
	err := s.inTx(ctx, func(q Querier) error {
		cur, err := livePoolForUpdate(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return ErrRunnerPoolStale
		}
		newLimits, err := marshalPoolLimits(limits)
		if err != nil {
			return err
		}
		oldLimits, err := marshalPoolLimits(cur.Limits)
		if err != nil {
			return err
		}
		if (name == nil || *name == cur.Name) && (state == nil || *state == cur.State) &&
			(limits == nil || bytes.Equal(newLimits, oldLimits)) && (recording == nil || *recording == cur.Recording) {
			out = cur
			return nil
		}
		out, err = scanRunnerPool(q.QueryRow(ctx, `
			UPDATE runner_pools SET name = COALESCE($2, name), state = COALESCE($3, state),
			       limits = COALESCE($4, limits), recording = COALESCE($5, recording), revision = revision + 1, updated_at = now()
			WHERE id = $1 RETURNING `+runnerPoolCols, id, name, stateText(state), newLimits, (*string)(recording)))
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

// DeleteRunnerPool tombstones a live pool, ends the runs waiting for it (KILLED, runner_pool_removed,
// their queue rows with them), drops its members, and clears the organisation default slots that name
// it, all in one transaction: ErrNotFound for an unknown or already deleted one,
// ErrRunnerPoolHasPolicy while it has a use policy. The row stays, so a run bound to it keeps its history.
func (s PG) DeleteRunnerPool(ctx context.Context, id uuid.UUID) (RunnerPoolDeletion, error) {
	var out RunnerPoolDeletion
	err := s.inTx(ctx, func(q Querier) error {
		var err error
		out, err = deleteRunnerPoolQ(ctx, q, id)
		return err
	})
	return out, err
}

func deleteRunnerPoolQ(ctx context.Context, q Querier, id uuid.UUID) (RunnerPoolDeletion, error) {
	var out RunnerPoolDeletion
	if _, err := livePoolForUpdate(ctx, q, id); err != nil {
		return out, err
	}
	var policies int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM runner_pool_use_policies WHERE pool_id = $1`, id).Scan(&policies); err != nil {
		return out, err
	}
	if policies > 0 {
		return out, ErrRunnerPoolHasPolicy
	}
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(array_agg(remote_runner_id ORDER BY remote_runner_id) FILTER (WHERE remote_runner_id IS NOT NULL), '{}'),
		       count(runner_id)
		FROM runner_pool_members WHERE pool_id = $1`, id).Scan(&out.ExecutorIDs, &out.RunnerCount); err != nil {
		return out, err
	}
	var err error
	if out.CancelledRuns, err = cancelPoolQueue(ctx, q, id, "queue: "+string(types.RunEndRunnerPoolRemoved), types.RunEndRunnerPoolRemoved.Sentence()); err != nil {
		return out, err
	}
	if _, err := q.Exec(ctx, `DELETE FROM runner_pool_members WHERE pool_id = $1`, id); err != nil {
		return out, err
	}
	if err := clearOrgDefaultSlotsQ(ctx, q, id, &out); err != nil {
		return out, err
	}
	out.Pool, err = scanRunnerPool(q.QueryRow(ctx, `
		UPDATE runner_pools SET state = 'deleted', revision = revision + 1, updated_at = now()
		WHERE id = $1 RETURNING `+runnerPoolCols, id))
	return out, err
}

// DeleteRunnerPoolsByOwner is a person's erasure of their pools: each live pool owner holds is deleted
// as DeleteRunnerPool deletes one, and then every pool of theirs, the tombstones included, forgets them
// (owner and created_by cleared). It holds the lock every create takes, so a pool the person creates
// meanwhile is either deleted here or made after it. The person's stored preferences are deleted in the
// same transaction. Returns the deletions, in pool id order.
func (s PG) DeleteRunnerPoolsByOwner(ctx context.Context, owner string) ([]RunnerPoolDeletion, error) {
	var out []RunnerPoolDeletion
	err := s.inTx(ctx, func(q Querier) error {
		out = nil
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('runner_pools.create'))`); err != nil {
			return err
		}
		ids, err := collect(ctx, q, "list", "owned runner pools",
			`SELECT id FROM runner_pools WHERE owner = $1 AND state <> 'deleted' ORDER BY id FOR UPDATE`, []any{owner},
			func(r pgx.Row) (uuid.UUID, error) {
				var id uuid.UUID
				return id, r.Scan(&id)
			})
		if err != nil {
			return err
		}
		for _, id := range ids {
			d, err := deleteRunnerPoolQ(ctx, q, id)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		if _, err = q.Exec(ctx, `UPDATE runner_pools SET owner = NULL, created_by = '' WHERE owner = $1 AND state = 'deleted'`, owner); err != nil {
			return err
		}
		// The person's stored preferences (their default pool among them) go with their pools.
		_, err = q.Exec(ctx, `DELETE FROM principal_prefs WHERE principal = $1`, owner)
		return err
	})
	return out, err
}

// clearOrgDefaultSlotsQ clears the organisation default when it names pool id and records the
// defaults before and after on out; it records nothing when it did not name it.
func clearOrgDefaultSlotsQ(ctx context.Context, q Querier, id uuid.UUID, out *RunnerPoolDeletion) error {
	before, err := scanOrgDefaults(q.QueryRow(ctx, `SELECT pool_id, barrier, recording, revision FROM runner_pool_org_defaults FOR UPDATE`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if before.PoolID == nil || *before.PoolID != id {
		return nil
	}
	if _, err := q.Exec(ctx, `UPDATE runner_pool_org_defaults SET pool_id = NULL, updated_by = 'system', updated_at = now(), revision = revision + 1`); err != nil {
		return err
	}
	after := before
	after.PoolID = nil
	after.Revision++ // the UPDATE above bumped it
	out.OrgDefaultsBefore, out.OrgDefaultsAfter = &before, &after
	return nil
}

// AddRunnerPoolExecutor adds a remote-hosted runner (executor is its id) to a live organization pool;
// added is false when it was already a member (no revision change). revision is the pool's revision
// after the call, read under the pool lock. ErrNotFound: no such live organization pool, or no such
// remote-hosted runner row.
func (s PG) AddRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string) (types.RunnerPoolMember, bool, int64, error) {
	var m types.RunnerPoolMember
	var added bool
	var rev int64
	err := s.inTx(ctx, func(q Querier) error {
		// The target lock a held change is decided under, so a stale check cannot apply over this write.
		if err := LockGovernanceTarget(ctx, q, types.GovernanceTargetRunnerPoolUsePolicy, id.String()); err != nil {
			return err
		}
		var err error
		m, added, rev, err = AddRunnerPoolExecutorQ(ctx, q, id, executor)
		return err
	})
	return m, added, rev, err
}

// AddRunnerPoolExecutorQ is AddRunnerPoolExecutor's statement on q (a pgx.Tx), which a held change
// applies through inside the decision transaction.
func AddRunnerPoolExecutorQ(ctx context.Context, q Querier, id uuid.UUID, executor string) (m types.RunnerPoolMember, added bool, rev int64, err error) {
	p, err := livePoolForUpdate(ctx, q, id)
	if err != nil || p.Scope != types.RunnerPoolOrganization {
		return m, false, 0, errOrNotFound(err)
	}
	return addRemoteMemberQ(ctx, q, p, executor)
}

// addRemoteMemberQ inserts remote-hosted runner rid into p (its row locked by the caller) and raises the
// revision when it was not a member yet. ErrNotFound: rid has no remote_runners row.
func addRemoteMemberQ(ctx context.Context, q Querier, p types.RunnerPool, rid string) (m types.RunnerPoolMember, added bool, rev int64, err error) {
	id := p.ID
	tag, err := q.Exec(ctx, `INSERT INTO runner_pool_members (pool_id, remote_runner_id) SELECT $1, id FROM remote_runners WHERE id = $2 ON CONFLICT DO NOTHING`, id, rid)
	if err != nil {
		return m, false, 0, err
	}
	rev = p.Revision
	if added = tag.RowsAffected() == 1; added {
		if rev, err = bumpRunnerPoolQ(ctx, q, id); err != nil {
			return m, false, 0, err
		}
	}
	err = q.QueryRow(ctx, `SELECT pool_id, kind, remote_runner_id, added_at FROM runner_pool_members WHERE pool_id = $1 AND remote_runner_id = $2`, id, rid).
		Scan(&m.PoolID, &m.Kind, &m.RemoteRunnerID, &m.AddedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, 0, ErrNotFound
	}
	return m, added, rev, err
}

// errOrNotFound is err, or ErrNotFound when err is nil: a pool of the wrong scope is not there.
func errOrNotFound(err error) error {
	if err != nil {
		return err
	}
	return ErrNotFound
}

// RemoveRunnerPoolExecutor removes a remote-hosted member of an organization pool; removed is false when it was not one. The caller
// decided on a catalogue read taken before the locks whether the detach is held; refuseGoverned says it
// decided "not held", and ErrRunnerPoolGoverned answers a use policy that was approved in between.
func (s PG) RemoveRunnerPoolExecutor(ctx context.Context, id uuid.UUID, executor string, refuseGoverned bool) (removed bool, rev int64, err error) {
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
				AND EXISTS (SELECT 1 FROM runner_pool_members WHERE pool_id = $1 AND remote_runner_id = $2)`, id, executor).Scan(&governed); err != nil {
				return err
			}
			if governed {
				return ErrRunnerPoolGoverned
			}
		}
		removed, rev, err = RemoveRunnerPoolExecutorQ(ctx, q, id, executor)
		return err
	})
	return removed, rev, err
}

// RemoveRunnerPoolExecutorQ is RemoveRunnerPoolExecutor's statement on q (a pgx.Tx), which a held
// detach applies through inside the decision transaction.
func RemoveRunnerPoolExecutorQ(ctx context.Context, q Querier, id uuid.UUID, executor string) (bool, int64, error) {
	return removeMemberQ(ctx, q, id, `DELETE FROM runner_pool_members m USING runner_pools p
		WHERE m.pool_id = $1 AND m.remote_runner_id = $2 AND p.id = m.pool_id AND p.scope = 'organization'`, executor)
}

// RemoveRemoteRunnerFromUserPool takes a remote-hosted runner out of owner's own user pool; removed is
// false when it was not a member. ErrNotFound: no such live pool of owner's.
func (s PG) RemoveRemoteRunnerFromUserPool(ctx context.Context, id uuid.UUID, remoteRunner, owner string) (bool, int64, error) {
	return s.removeMember(ctx, id, `
		DELETE FROM runner_pool_members m USING runner_pools p
		WHERE m.pool_id = $1 AND m.remote_runner_id = $2 AND p.id = m.pool_id AND p.scope = 'user' AND p.owner = $3`, remoteRunner, owner)
}

// AddRemoteRunnerToUserPool adds a remote-hosted runner to owner's own live user pool. The caller
// checked the runner's people policy at peopleRevision; the runner row is read FOR SHARE here, so a
// policy write (which takes it FOR UPDATE and raises its revision) cannot slip between that check and
// this insert: a moved revision is ErrRunnerPoolStale. ErrNotFound: the pool is not owner's live user
// pool, or the runner has no row.
func (s PG) AddRemoteRunnerToUserPool(ctx context.Context, id uuid.UUID, remoteRunner, owner string, peopleRevision int64) (types.RunnerPoolMember, bool, int64, error) {
	var m types.RunnerPoolMember
	var added bool
	var rev int64
	err := s.inTx(ctx, func(q Querier) error {
		p, err := livePoolForUpdate(ctx, q, id)
		if err != nil || p.Scope != types.RunnerPoolUser || p.Owner != owner {
			return errOrNotFound(err)
		}
		var cur int64
		err = q.QueryRow(ctx, `SELECT revision FROM remote_runners WHERE id = $1 FOR SHARE`, remoteRunner).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur != peopleRevision {
			return ErrRunnerPoolStale
		}
		m, added, rev, err = addRemoteMemberQ(ctx, q, p, remoteRunner)
		return err
	})
	return m, added, rev, err
}

// LockLivePoolQ takes the pool row lock of a live pool on q, ErrNotFound when there is none. A held
// change takes it right after its target lock, so every member write waits for the decision.
func LockLivePoolQ(ctx context.Context, q Querier, id uuid.UUID) error {
	_, err := livePoolForUpdate(ctx, q, id)
	return err
}

// RemoveOwnRunnerFromPool removes owner's own runner (in any state, so a revoked one can be tidied
// away) from a pool; removed is false when it was not a member or not owner's.
func (s PG) RemoveOwnRunnerFromPool(ctx context.Context, id, runnerID uuid.UUID, owner string) (bool, int64, error) {
	return s.removeMember(ctx, id, `
		DELETE FROM runner_pool_members m USING runners r
		WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.id = m.runner_id AND r.owner = $3`, runnerID, owner)
}

// removeMember runs one member DELETE under the pool lock and raises the revision when it removed a row.
func (s PG) removeMember(ctx context.Context, id uuid.UUID, stmt string, args ...any) (removed bool, rev int64, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		removed, rev, err = removeMemberQ(ctx, q, id, stmt, args...)
		return err
	})
	return removed, rev, err
}

func removeMemberQ(ctx context.Context, q Querier, id uuid.UUID, stmt string, args ...any) (removed bool, rev int64, err error) {
	p, err := livePoolForUpdate(ctx, q, id)
	if err != nil {
		return false, 0, err
	}
	tag, err := q.Exec(ctx, stmt, append([]any{id}, args...)...)
	if err != nil {
		return false, 0, err
	}
	rev = p.Revision
	if removed = tag.RowsAffected() == 1; removed {
		rev, err = bumpRunnerPoolQ(ctx, q, id)
	}
	return removed, rev, err
}

// AddOwnRunnerToPool adds a self-hosted runner to a live user pool only when owner owns both the pool
// and the runner, the runner is claimed and bound to this organisation's URL, all decided in the insert
// itself so a revocation or a transfer cannot slip between a check and the write. An organization pool
// is ErrRunnerPoolScope: it takes remote-hosted runners only. ErrNotFound covers every other case alike:
// an unknown pool or runner, and a pool or a runner that is not the owner's.
func (s PG) AddOwnRunnerToPool(ctx context.Context, id, runnerID uuid.UUID, owner, orgURLSHA256 string) (types.RunnerPoolMember, bool, int64, error) {
	var m types.RunnerPoolMember
	var added bool
	var rev int64
	err := s.inTx(ctx, func(q Querier) error {
		p, err := livePoolForUpdate(ctx, q, id)
		if err != nil {
			return err
		}
		if p.Scope != types.RunnerPoolUser {
			return ErrRunnerPoolScope
		}
		m.RunnerID = &runnerID
		err = q.QueryRow(ctx, `
			INSERT INTO runner_pool_members (pool_id, runner_id)
			SELECT p.id, r.id FROM runner_pools p, runners r
			WHERE p.id = $1 AND r.id = $2 AND p.owner = $3 AND r.owner = $3 AND r.owner_kind = 'person' AND r.state = 'claimed' AND r.org_url_sha256 = $4
			ON CONFLICT DO NOTHING RETURNING pool_id, kind, added_at`, id, runnerID, owner, orgURLSHA256).Scan(&m.PoolID, &m.Kind, &m.AddedAt)
		if err == nil {
			added = true
			rev, err = bumpRunnerPoolQ(ctx, q, id)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Not inserted: already the owner's member, or not theirs to add.
		rev = p.Revision
		err = q.QueryRow(ctx, `
			SELECT m.pool_id, m.kind, m.added_at FROM runner_pool_members m JOIN runners r ON r.id = m.runner_id
			WHERE m.pool_id = $1 AND m.runner_id = $2 AND r.owner = $3 AND $3 = $4`, id, runnerID, owner, p.Owner).Scan(&m.PoolID, &m.Kind, &m.AddedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return m, added, rev, err
}

// RunnerPoolExecutorIDsQ lists a pool's remote-hosted members, sorted, never nil.
func RunnerPoolExecutorIDsQ(ctx context.Context, q Querier, id uuid.UUID) ([]string, error) {
	return collect(ctx, q, "list", "runner pool remote runners", `SELECT remote_runner_id FROM runner_pool_members WHERE pool_id = $1 AND remote_runner_id IS NOT NULL ORDER BY remote_runner_id`,
		[]any{id}, func(r pgx.Row) (string, error) {
			var v string
			return v, r.Scan(&v)
		})
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

// PutRunnerPoolUsePolicy sets a live organization pool's use policy and raises the pool's revision;
// a non-nil revision must be the pool's, checked under the pool lock (ErrRunnerPoolStale).
// It takes the governance target lock an approval of a held change takes too.
func (s PG) PutRunnerPoolUsePolicy(ctx context.Context, id uuid.UUID, revision *int64, subjects []types.RunnerPoolSubject, by string) (types.RunnerPoolUsePolicy, error) {
	var out types.RunnerPoolUsePolicy
	err := s.inTx(ctx, func(q Querier) error {
		if err := LockGovernanceTarget(ctx, q, types.GovernanceTargetRunnerPoolUsePolicy, id.String()); err != nil {
			return err
		}
		var err error
		out, err = PutRunnerPoolUsePolicyQ(ctx, q, id, revision, subjects, by)
		return err
	})
	return out, err
}

// PutRunnerPoolUsePolicyQ is PutRunnerPoolUsePolicy's statement on q (a pgx.Tx, the pool row locked
// here). ErrNotFound: no such live organization pool.
func PutRunnerPoolUsePolicyQ(ctx context.Context, q Querier, id uuid.UUID, revision *int64, subjects []types.RunnerPoolSubject, by string) (types.RunnerPoolUsePolicy, error) {
	p, err := livePoolForUpdate(ctx, q, id)
	if err != nil || p.Scope != types.RunnerPoolOrganization {
		return types.RunnerPoolUsePolicy{}, errOrNotFound(err)
	}
	if revision != nil && *revision != p.Revision {
		return types.RunnerPoolUsePolicy{}, ErrRunnerPoolStale
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
	d, err := scanOrgDefaults(s.Pool.QueryRow(ctx, `SELECT pool_id, barrier, recording, revision FROM runner_pool_org_defaults`))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPoolDefaults{}, nil
	}
	return d, err
}

// PutRunnerPoolOrgDefaults replaces the organisation's defaults and returns what they were and what it
// wrote, both in one transaction under a row lock so two concurrent writes each see the other's result
// as their before. An empty Recording keeps the stored one, decided against the locked row; preferred
// lowers it. Every write bumps the revision. d.Revision, when set, is the revision the writer read: a
// different one is ErrRunnerPoolStale, checked under the row lock, and zero writes unconditionally (a
// writer that read no row, or one that does not say). The caller has checked the pool's state; a
// default only names one, and the resolver refuses a name that has since been switched off. A pool
// deleted between that check and this write is ErrNotFound: the pool row is locked first, as a delete
// locks it before it clears the default.
func (s PG) PutRunnerPoolOrgDefaults(ctx context.Context, d types.RunnerPoolDefaults, by string) (before, after types.RunnerPoolDefaults, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		if d.PoolID != nil {
			if _, err := livePoolForUpdate(ctx, q, *d.PoolID); err != nil {
				return err
			}
		}
		// The first write ever has no row to lock, so two of them would each audit a zero before. A
		// seed at revision 0 (what Get reports when nothing was written) gives them one to queue on.
		if _, err := q.Exec(ctx, `INSERT INTO runner_pool_org_defaults (singleton, revision) VALUES (TRUE, 0) ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		var err error
		if before, err = scanOrgDefaults(q.QueryRow(ctx, `SELECT pool_id, barrier, recording, revision FROM runner_pool_org_defaults FOR UPDATE`)); err != nil {
			return err
		}
		if d.Revision != 0 && d.Revision != before.Revision {
			return ErrRunnerPoolStale
		}
		after, err = scanOrgDefaults(q.QueryRow(ctx, `
		INSERT INTO runner_pool_org_defaults (pool_id, barrier, recording, updated_by) VALUES ($1,$2,$3,$4)
		ON CONFLICT (singleton) DO UPDATE SET pool_id = EXCLUDED.pool_id, barrier = EXCLUDED.barrier,
		       recording = COALESCE(NULLIF(EXCLUDED.recording, ''), runner_pool_org_defaults.recording),
		       updated_by = EXCLUDED.updated_by, updated_at = now(), revision = runner_pool_org_defaults.revision + 1
		RETURNING pool_id, barrier, recording, revision`,
			d.PoolID, string(d.Barrier), string(d.Recording), by))
		return err
	})
	return before, after, err
}

// BootstrapRunnerPools is the one-time upgrade step: when this deployment has configured remote-hosted
// runners and no organization pool has ever existed (a deleted one still counts, so an administrator's
// deletion sticks), it creates one organization pool named name holding exactly those runners and makes
// it the organisation's default pool, so a launch that names no pool still resolves. Defaults the
// organisation saved before any pool existed keep their barrier. It gives nobody a runner they lacked
// (no use policy, and no user pool is ever made); it is still a placement change, since a run may then
// land on any member that advertises its class. With no runners or a catalogue that has existed it does
// nothing. Each runner's row is EnsureRemoteRunners'. Returns the pool it created and the organisation
// defaults it left, both nil when it did nothing.
func (s PG) BootstrapRunnerPools(ctx context.Context, executors []string, name string) (*types.RunnerPool, *types.RunnerPoolDefaults, error) {
	if len(executors) == 0 {
		return nil, nil, nil
	}
	var created *types.RunnerPool
	var defaults *types.RunnerPoolDefaults
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('runner_pools.create'))`); err != nil {
			return err
		}
		var exists bool
		// A person's own user pool says nothing about the organisation's catalogue.
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pools WHERE scope = 'organization')`).Scan(&exists); err != nil || exists {
			return err
		}
		var p types.RunnerPool
		if err := insertRunnerPoolQ(ctx, q, types.RunnerPool{ID: uuid.New(), Name: name, Scope: types.RunnerPoolOrganization}, "system", &p); err != nil {
			return err
		}
		for _, e := range executors {
			if _, err := q.Exec(ctx, `INSERT INTO runner_pool_members (pool_id, remote_runner_id) SELECT $1, id FROM remote_runners WHERE id = $2 ON CONFLICT DO NOTHING`, p.ID, e); err != nil {
				return err
			}
		}
		d, err := scanOrgDefaults(q.QueryRow(ctx, `
			INSERT INTO runner_pool_org_defaults (pool_id, updated_by) VALUES ($1, 'system')
			ON CONFLICT (singleton) DO UPDATE SET pool_id = EXCLUDED.pool_id, updated_by = 'system', updated_at = now(),
			       revision = runner_pool_org_defaults.revision + 1
			RETURNING pool_id, barrier, recording, revision`, p.ID))
		if err != nil {
			return err
		}
		created, defaults = &p, &d
		return nil
	})
	return created, defaults, err
}

func scanOrgDefaults(row pgx.Row) (types.RunnerPoolDefaults, error) {
	var d types.RunnerPoolDefaults
	var barrier, recording string
	err := row.Scan(&d.PoolID, &barrier, &recording, &d.Revision)
	d.Barrier, d.Recording = types.ConfinementClass(barrier), types.RecordingRequirement(recording)
	return d, err
}
