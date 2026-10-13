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
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RemoteRunnerStore is the optional store capability behind the remote-hosted runner routes.
type RemoteRunnerStore interface {
	// EnsureRemoteRunners inserts a row, with its default name and the limits proposals holds for it,
	// for each id that has none, and returns the rows it inserted (made). A row that already exists and
	// declares no limits gets its proposal (proposed); one that declares limits keeps them, since they
	// are an administrator's from the first boot on.
	EnsureRemoteRunners(ctx context.Context, ids []string, proposals map[string]types.RunnerLimits) (made, proposed []types.RemoteRunner, err error)
	ListRemoteRunners(ctx context.Context) ([]RemoteRunnerRow, error)
	GetRemoteRunner(ctx context.Context, id string) (RemoteRunnerRow, error)
	// UpdateRemoteRunner changes the display name, the limits or both at the revision the caller read,
	// and returns the runner before and after.
	UpdateRemoteRunner(ctx context.Context, id string, revision int64, name *string, limits *types.RunnerLimits, by string) (before, after types.RemoteRunner, err error)
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

const remoteRunnerCols = `id, name, people_pools, people_subjects, revision, updated_by, created_at, updated_at, max_runs, ` + runnerLimitCols

func scanRemoteRunner(row pgx.Row, extra ...any) (types.RemoteRunner, error) {
	rr := types.RemoteRunner{Kind: types.RunnerKindRemoteHosted}
	var pools string
	var subjects []byte
	var runs *int
	var lim limitCols
	targets := append([]any{&rr.ID, &rr.Name, &pools, &subjects, &rr.Revision, &rr.UpdatedBy, &rr.CreatedAt, &rr.UpdatedAt, &runs}, lim.targets()...)
	err := row.Scan(append(targets, extra...)...)
	rr.Limits = lim.limits(runs)
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

func (s PG) EnsureRemoteRunners(ctx context.Context, ids []string, proposals map[string]types.RunnerLimits) (made, proposed []types.RemoteRunner, err error) {
	for _, id := range ids {
		var lim *types.RunnerLimits
		if p, ok := proposals[id]; ok {
			lim = &p
		}
		rr, err := scanRemoteRunner(s.Pool.QueryRow(ctx, `
			INSERT INTO remote_runners (id, name, updated_by, max_runs, `+runnerLimitCols+`) VALUES ($1, $2, 'system', $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (id) DO NOTHING RETURNING `+remoteRunnerCols, append([]any{id, types.RemoteRunnerDefaultName(id), runsOf(lim)}, limitArgs(lim)...)...))
		switch {
		case err == nil:
			made = append(made, rr)
			continue
		case !errors.Is(err, ErrNotFound):
			return nil, nil, err
		case lim == nil:
			continue
		}
		rr, err = scanRemoteRunner(s.Pool.QueryRow(ctx, `UPDATE remote_runners SET max_runs = $2, (`+runnerLimitCols+`) = ($3, $4, $5, $6, $7, $8),
			revision = revision + 1, updated_by = 'system', updated_at = now()
			WHERE id = $1 AND max_runs IS NULL RETURNING `+remoteRunnerCols, append([]any{id, lim.RunsAtOnce}, limitArgs(lim)...)...))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		proposed = append(proposed, rr)
	}
	return made, proposed, nil
}

// runsOf is l's Runs at once, nil for no limits.
func runsOf(l *types.RunnerLimits) *int {
	if l == nil {
		return nil
	}
	return &l.RunsAtOnce
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

func (s PG) UpdateRemoteRunner(ctx context.Context, id string, revision int64, name *string, limits *types.RunnerLimits, by string) (before, after types.RemoteRunner, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		if before, err = GetRemoteRunnerQ(ctx, q, id, true); err != nil {
			return err
		}
		if before.Revision != revision {
			return ErrRunnerPoolStale
		}
		newName, newLimits := before.Name, before.Limits
		if name != nil {
			newName = *name
		}
		if limits != nil {
			newLimits = limits
		}
		if newName == before.Name && reflect.DeepEqual(newLimits, before.Limits) {
			after = before
			return nil
		}
		after, err = scanRemoteRunner(q.QueryRow(ctx, `UPDATE remote_runners SET name = $2, max_runs = $3, (`+runnerLimitCols+`) = ($4, $5, $6, $7, $8, $9),
			revision = revision + 1, updated_by = $10, updated_at = now()
			WHERE id = $1 RETURNING `+remoteRunnerCols, append(append([]any{id, newName, runsOf(newLimits)}, limitArgs(newLimits)...), by)...))
		return err
	})
	return before, after, err
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

// StampServerRun records the remote-hosted runner a run Wardyn launched itself (a sign-in, a probe, a
// source scan) is dispatched to, and what it reserves there, so it counts in that runner's In use. Such
// a run is not placed through a pool and is not held to the runner's limits; it only counts. A run
// already bound to a runner of either kind is left as it is.
func (s PG) StampServerRun(ctx context.Context, id uuid.UUID, remote string, reserved types.RunnerAmounts) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET runner_pool_executor = $2, `+
		`reserved_cpu_millis = $3, reserved_memory_mib = $4, reserved_disk_mib = $5
		WHERE id = $1 AND runner_id IS NULL AND runner_pool_executor = ''`,
		id, remote, reserved.CPUMillis, reserved.MemoryMiB, reserved.DiskMiB); err != nil {
		return fmt.Errorf("store: stamp server run: %w", err)
	}
	return nil
}
