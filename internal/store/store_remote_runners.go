// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Remote-hosted runners (migration 0173): the server's own Docker host or Kubernetes cluster, one row
// per configured substrate name, with a display name and the "Available to people's pools" policy.
// Every write takes the row FOR UPDATE and raises its revision; a person's add into their own user
// pool reads it FOR SHARE at the revision whose policy was checked (AddRemoteRunnerToUserPool).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RemoteRunnerStore is the optional store capability behind the remote-hosted runner routes.
type RemoteRunnerStore interface {
	// EnsureRemoteRunners inserts a row, with its default name, for each id that has none, and returns
	// the rows it inserted.
	EnsureRemoteRunners(ctx context.Context, ids []string) ([]types.RemoteRunner, error)
	ListRemoteRunners(ctx context.Context) ([]RemoteRunnerRow, error)
	GetRemoteRunner(ctx context.Context, id string) (RemoteRunnerRow, error)
	// RenameRemoteRunner changes the display name at the revision the caller read.
	RenameRemoteRunner(ctx context.Context, id string, revision int64, name, by string) (types.RemoteRunner, error)
	// SetRemoteRunnerPeople writes the people policy under the governance target lock a held change's
	// decision takes, and returns the runner before and after.
	SetRemoteRunnerPeople(ctx context.Context, id string, revision *int64, pools types.RemoteRunnerPeoplePools, subjects []types.RunnerPoolSubject, by string) (before, after types.RemoteRunner, err error)
}

var _ RemoteRunnerStore = PG{}

// RemoteRunnerRow is a remote-hosted runner with every live pool it is in, user pools included.
type RemoteRunnerRow struct {
	Runner types.RemoteRunner
	Pools  []types.RemoteRunnerPoolRef
}

const remoteRunnerCols = `id, name, people_pools, people_subjects, revision, updated_by, created_at, updated_at`

func scanRemoteRunner(row pgx.Row, extra ...any) (types.RemoteRunner, error) {
	rr := types.RemoteRunner{Kind: types.RunnerKindRemoteHosted}
	var pools string
	var subjects []byte
	err := row.Scan(append([]any{&rr.ID, &rr.Name, &pools, &subjects, &rr.Revision, &rr.UpdatedBy, &rr.CreatedAt, &rr.UpdatedAt}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return rr, ErrNotFound
	}
	if err != nil {
		return rr, fmt.Errorf("store: scan remote runner: %w", err)
	}
	rr.PeoplePools = types.RemoteRunnerPeoplePools(pools)
	if subjects != nil {
		rr.PeopleSubjects = []types.RunnerPoolSubject{}
		if err := json.Unmarshal(subjects, &rr.PeopleSubjects); err != nil {
			return rr, fmt.Errorf("store: unmarshal remote runner people: %w", err)
		}
	}
	return rr, nil
}

func (s PG) EnsureRemoteRunners(ctx context.Context, ids []string) ([]types.RemoteRunner, error) {
	var out []types.RemoteRunner
	for _, id := range ids {
		rr, err := scanRemoteRunner(s.Pool.QueryRow(ctx, `
			INSERT INTO remote_runners (id, name, updated_by) VALUES ($1, $2, 'system')
			ON CONFLICT (id) DO NOTHING RETURNING `+remoteRunnerCols, id, types.RemoteRunnerDefaultName(id)))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rr)
	}
	return out, nil
}

// remoteRunnerPoolsSQL is every live pool holding remote runner $1, by scope then name.
const remoteRunnerPoolsSQL = `
	COALESCE((SELECT jsonb_agg(jsonb_build_object('id', p.id, 'name', p.name, 'scope', p.scope, 'owner', COALESCE(p.owner, ''))
	                           ORDER BY p.scope, lower(p.name), p.id)
	          FROM runner_pool_members m JOIN runner_pools p ON p.id = m.pool_id
	          WHERE m.remote_runner_id = rr.id AND p.state <> 'deleted'), '[]'::jsonb)`

func scanRemoteRunnerRow(row pgx.Row) (RemoteRunnerRow, error) {
	var pools []byte
	rr, err := scanRemoteRunner(row, &pools)
	if err != nil {
		return RemoteRunnerRow{}, err
	}
	out := RemoteRunnerRow{Runner: rr}
	if err := json.Unmarshal(pools, &out.Pools); err != nil {
		return RemoteRunnerRow{}, fmt.Errorf("store: unmarshal remote runner pools: %w", err)
	}
	return out, nil
}

func (s PG) ListRemoteRunners(ctx context.Context) ([]RemoteRunnerRow, error) {
	return collect(ctx, s.Pool, "list", "remote runners",
		`SELECT `+remoteRunnerCols+`, `+remoteRunnerPoolsSQL+` FROM remote_runners rr ORDER BY id`, nil, scanRemoteRunnerRow)
}

func (s PG) GetRemoteRunner(ctx context.Context, id string) (RemoteRunnerRow, error) {
	return scanRemoteRunnerRow(s.Pool.QueryRow(ctx, `SELECT `+remoteRunnerCols+`, `+remoteRunnerPoolsSQL+` FROM remote_runners rr WHERE id = $1`, id))
}

// GetRemoteRunnerQ reads one remote runner on q, locked FOR UPDATE when forUpdate.
func GetRemoteRunnerQ(ctx context.Context, q Querier, id string, forUpdate bool) (types.RemoteRunner, error) {
	return scanRemoteRunner(q.QueryRow(ctx, `SELECT `+remoteRunnerCols+` FROM remote_runners WHERE id = $1`+lockSuffix(forUpdate), id))
}

func (s PG) RenameRemoteRunner(ctx context.Context, id string, revision int64, name, by string) (types.RemoteRunner, error) {
	var out types.RemoteRunner
	err := s.inTx(ctx, func(q Querier) error {
		cur, err := GetRemoteRunnerQ(ctx, q, id, true)
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return ErrRunnerPoolStale
		}
		if cur.Name == name {
			out = cur
			return nil
		}
		out, err = scanRemoteRunner(q.QueryRow(ctx, `UPDATE remote_runners SET name = $2, revision = revision + 1, updated_by = $3, updated_at = now()
			WHERE id = $1 RETURNING `+remoteRunnerCols, id, name, by))
		return err
	})
	return out, err
}

// SetRemoteRunnerPeopleQ writes a remote runner's people policy on q (a pgx.Tx holding the governance
// target lock): pools off clears the list, listed stores subjects. A non-nil revision must be the
// runner's (ErrRunnerPoolStale). ErrNotFound: no such runner.
func SetRemoteRunnerPeopleQ(ctx context.Context, q Querier, id string, revision *int64, pools types.RemoteRunnerPeoplePools, subjects []types.RunnerPoolSubject, by string) (types.RemoteRunner, error) {
	cur, err := GetRemoteRunnerQ(ctx, q, id, true)
	if err != nil {
		return cur, err
	}
	if revision != nil && *revision != cur.Revision {
		return cur, ErrRunnerPoolStale
	}
	var raw []byte
	if pools == types.PeoplePoolsListed {
		if raw, err = json.Marshal(subjects); err != nil {
			return cur, err
		}
	}
	return scanRemoteRunner(q.QueryRow(ctx, `UPDATE remote_runners SET people_pools = $2, people_subjects = $3, revision = revision + 1,
		updated_by = $4, updated_at = now() WHERE id = $1 RETURNING `+remoteRunnerCols, id, string(pools), raw, by))
}

func (s PG) SetRemoteRunnerPeople(ctx context.Context, id string, revision *int64, pools types.RemoteRunnerPeoplePools, subjects []types.RunnerPoolSubject, by string) (before, after types.RemoteRunner, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		if err := LockGovernanceTarget(ctx, q, types.GovernanceTargetRemoteRunnerPeople, id); err != nil {
			return err
		}
		var err error
		if before, err = GetRemoteRunnerQ(ctx, q, id, true); err != nil {
			return err
		}
		after, err = SetRemoteRunnerPeopleQ(ctx, q, id, revision, pools, subjects, by)
		return err
	})
	return before, after, err
}
