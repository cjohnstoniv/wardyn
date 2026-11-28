// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunnerHolderStore records which replica holds each registered runner's session, so a call on another replica
// reaches the runner through that one, and which replica each server process's watcher lease names.
type RunnerHolderStore interface {
	ClaimRunnerSessionHolder(ctx context.Context, runnerID, sessionID uuid.UUID, replicaID, peerURL string) (bool, error)
	RunnerSessionHolders(ctx context.Context) ([]RunnerSessionHolderRow, error)
	TouchRunnerSessionHolders(ctx context.Context, replicaID string, sessions []uuid.UUID) ([]uuid.UUID, error)
	DropRunnerSessionHolder(ctx context.Context, runnerID, sessionID uuid.UUID) error
	RunnerSessionHolder(ctx context.Context, runnerID uuid.UUID) (PeerRouteRow, bool, error)
	TouchRunnerPeerReplica(ctx context.Context, leaseOwner, replicaID, peerURL string) error
	RunOutputLauncher(ctx context.Context, runID uuid.UUID) (RunOutputLauncherRow, error)
}

var _ RunnerHolderStore = PG{}

// RunnerSessionHolderRow is one fresh holder row.
type RunnerSessionHolderRow struct {
	RunnerID  uuid.UUID
	SessionID uuid.UUID
	ReplicaID string
	PeerURL   string
}

// PeerRouteRow is where a runner's session is held: the replica, its internal address and the session.
type PeerRouteRow struct {
	SessionID uuid.UUID
	ReplicaID string
	PeerURL   string
}

// LauncherState is what RunOutputLauncher found for a run's output.
type LauncherState int

const (
	// LauncherNone: no such run, or it ended or was lost; its output is refused.
	LauncherNone LauncherState = iota
	// LauncherWait: the lease or the replica row is stale; the output is answered as capacity for now.
	LauncherWait
	// LauncherFresh: a fresh lease whose process has a fresh replica row.
	LauncherFresh
)

// RunOutputLauncherRow is the replica that holds a run's watcher lease, read for its runner's output.
type RunOutputLauncherRow struct {
	State        LauncherState
	ReplicaID    string
	PeerURL      string
	WatcherOwner string
	LeaseFresh   bool
}

// holderFresh is how long a holder or replica row is routed to after its last touch.
const holderFresh = `interval '30 seconds'`

// ClaimRunnerSessionHolder records this replica as the holder of the runner's session, only while
// runner_inventories still names that session. The inventory row is read FOR SHARE, so a newer session's
// BeginRunnerInventory (FOR UPDATE) waits for this claim, and a claim that starts after it finds another session
// and writes nothing. It reports whether it wrote.
func (s PG) ClaimRunnerSessionHolder(ctx context.Context, runnerID, sessionID uuid.UUID, replicaID, peerURL string) (bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transaction is closed
	var current uuid.UUID
	err = tx.QueryRow(ctx, `SELECT session_id FROM runner_inventories WHERE runner_id=$1 FOR SHARE`, runnerID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current != sessionID {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runner_session_holders(runner_id, session_id, replica_id, peer_url, seen_at) VALUES($1,$2,$3,$4,now()) ON CONFLICT (runner_id) DO UPDATE SET session_id=EXCLUDED.session_id, replica_id=EXCLUDED.replica_id, peer_url=EXCLUDED.peer_url, seen_at=now()`,
		runnerID, sessionID, replicaID, peerURL); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// RunnerSessionHolders lists every holder row seen in the last 30 seconds.
func (s PG) RunnerSessionHolders(ctx context.Context) ([]RunnerSessionHolderRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT runner_id, session_id, replica_id, peer_url FROM runner_session_holders WHERE seen_at > now() - `+holderFresh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunnerSessionHolderRow
	for rows.Next() {
		var r RunnerSessionHolderRow
		if err := rows.Scan(&r.RunnerID, &r.SessionID, &r.ReplicaID, &r.PeerURL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchRunnerSessionHolders refreshes seen_at on this replica's rows for the given sessions (those whose link is
// up now) and returns the sessions it touched. A session it did not return has no row here.
func (s PG) TouchRunnerSessionHolders(ctx context.Context, replicaID string, sessions []uuid.UUID) ([]uuid.UUID, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `UPDATE runner_session_holders SET seen_at=now() WHERE replica_id=$1 AND session_id = ANY($2) RETURNING session_id`, replicaID, sessions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DropRunnerSessionHolder deletes the runner's row only while it still names this session, so an old session's end
// never removes the row of the session that replaced it on another replica.
func (s PG) DropRunnerSessionHolder(ctx context.Context, runnerID, sessionID uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM runner_session_holders WHERE runner_id=$1 AND session_id=$2`, runnerID, sessionID)
	return err
}

// RunnerSessionHolder reads the runner's holder row when it was seen in the last 30 seconds.
func (s PG) RunnerSessionHolder(ctx context.Context, runnerID uuid.UUID) (PeerRouteRow, bool, error) {
	var r PeerRouteRow
	err := s.Pool.QueryRow(ctx, `SELECT session_id, replica_id, peer_url FROM runner_session_holders WHERE runner_id=$1 AND seen_at > now() - `+holderFresh, runnerID).
		Scan(&r.SessionID, &r.ReplicaID, &r.PeerURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return PeerRouteRow{}, false, nil
	}
	if err != nil {
		return PeerRouteRow{}, false, err
	}
	return r, true, nil
}

// peerReplicaPrune is when this process last deleted the replica rows unseen for a day.
var peerReplicaPrune struct {
	mu   sync.Mutex
	last time.Time
}

// TouchRunnerPeerReplica upserts this process's runner_peer_replicas row. On its first call, and then at most once
// an hour, it also deletes the rows unseen for a day, so the table holds the processes seen in the last day.
func (s PG) TouchRunnerPeerReplica(ctx context.Context, leaseOwner, replicaID, peerURL string) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO runner_peer_replicas(lease_owner, replica_id, peer_url, seen_at) VALUES($1,$2,$3,now()) ON CONFLICT (lease_owner) DO UPDATE SET replica_id=EXCLUDED.replica_id, peer_url=EXCLUDED.peer_url, seen_at=now()`,
		leaseOwner, replicaID, peerURL); err != nil {
		return err
	}
	peerReplicaPrune.mu.Lock()
	due := peerReplicaPrune.last.IsZero() || time.Since(peerReplicaPrune.last) >= time.Hour
	peerReplicaPrune.mu.Unlock()
	if !due {
		return nil
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM runner_peer_replicas WHERE seen_at < now() - interval '1 day'`); err != nil {
		return err
	}
	peerReplicaPrune.mu.Lock()
	peerReplicaPrune.last = time.Now()
	peerReplicaPrune.mu.Unlock()
	return nil
}

// RunOutputLauncher reads the replica that holds the run's watcher lease. A run that is gone, ended or lost is
// LauncherNone; a lease within 90 seconds whose process has a replica row within 30 seconds is LauncherFresh;
// anything else is LauncherWait.
func (s PG) RunOutputLauncher(ctx context.Context, runID uuid.UUID) (RunOutputLauncherRow, error) {
	var (
		state                string
		notLost              bool
		owner, replica, url  *string
		leaseFresh, rowFresh *bool
	)
	err := s.Pool.QueryRow(ctx, `SELECT r.state, r.lost_at IS NULL, r.watcher_owner, r.watcher_heartbeat > now() - interval '90 seconds', p.replica_id, p.peer_url, p.seen_at > now() - `+holderFresh+` FROM agent_runs r LEFT JOIN runner_peer_replicas p ON p.lease_owner = r.watcher_owner WHERE r.id = $1`, runID).
		Scan(&state, &notLost, &owner, &leaseFresh, &replica, &url, &rowFresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunOutputLauncherRow{State: LauncherNone}, nil
	}
	if err != nil {
		return RunOutputLauncherRow{}, err
	}
	row := RunOutputLauncherRow{State: LauncherWait, LeaseFresh: leaseFresh != nil && *leaseFresh}
	if owner != nil {
		row.WatcherOwner = *owner
	}
	if !nonTerminalRunState(state) || !notLost {
		row.State = LauncherNone
		return row, nil
	}
	if row.LeaseFresh && rowFresh != nil && *rowFresh {
		row.State, row.ReplicaID, row.PeerURL = LauncherFresh, *replica, *url
	}
	return row, nil
}

// nonTerminalRunState reads nonTerminalRunStates, the sweeps' SQL list, so the two never disagree.
func nonTerminalRunState(state string) bool {
	return state != "" && strings.Contains(nonTerminalRunStates, "'"+state+"'")
}
