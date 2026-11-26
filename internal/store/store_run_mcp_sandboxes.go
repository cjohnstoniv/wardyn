// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An MCP server of a run that starts from a command runs in a sandbox of its own, one row of
// run_mcp_sandboxes each (migration 0428). Every write that moves a row's state is one UPDATE that checks
// the row, and where a run's state decides, the run too, in the same statement, so a kill, a loss or an end
// that lands first makes it answer false and its caller kills what it started.

// The states of an MCP sandbox row.
const (
	MCPSandboxStarting   = "starting"
	MCPSandboxReady      = "ready"
	MCPSandboxRestarting = "restarting"
	MCPSandboxStopped    = "stopped"
	MCPSandboxFailed     = "failed"
)

// RunMCPSandbox is one row of run_mcp_sandboxes.
type RunMCPSandbox struct {
	RunID   uuid.UUID
	Ordinal int // the server's run_components ordinal
	// ServerKey is the server's key: the <server> of mcp__<server>__<tool>, and the mcp_sandbox label of
	// every row its proxy posts.
	ServerKey  string
	SandboxRef string
	State      string
	// Restarts counts the restarts since RestartWindowStart.
	Restarts           int
	RestartWindowStart time.Time
	// LastCause is the cause of the row's last move to stopped or failed, "" after a reopen.
	LastCause    string
	PolicyDigest string
	// Reserved is the CPU and memory the sandbox and its proxy hold.
	Reserved          types.RunnerAmounts
	ProxyConfigSealed []byte
	// RelayAddr is the MCP proxy's pinned address on the run's relay link; RelayTokenSHA256 the hex SHA-256
	// of the relay token, which is never stored.
	RelayAddr        string
	RelayTokenSHA256 string
	// CPTokenJTI is the jti of the MCP proxy's control-plane token. PrevCPTokenJTI is the one it replaced,
	// still served until PrevCPTokenUntil, so the rows a replaced proxy flushes still land.
	CPTokenJTI       string
	PrevCPTokenJTI   string
	PrevCPTokenUntil *time.Time
	// GrantIDs are the server's own grants, the only ones its proxy may resolve.
	GrantIDs  []uuid.UUID
	StoppedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RunMCPSandboxStore keeps a run's MCP sandbox rows. Optional like RunHarnessStore: callers type-assert,
// and a caller that cannot reach it refuses, never serves an MCP sandbox without its row.
type RunMCPSandboxStore interface {
	// InsertRunMCPSandbox writes dispatch's row for one server, as starting, before its sandbox is created.
	InsertRunMCPSandbox(ctx context.Context, row RunMCPSandbox) error
	// ListRunMCPSandboxes is the run's rows in ordinal order, at most one per server.
	ListRunMCPSandboxes(ctx context.Context, runID uuid.UUID) ([]RunMCPSandbox, error)
	// MarkMCPSandboxStartedIfStarting is dispatch's move of a started row to ready, only while the run is
	// STARTING.
	MarkMCPSandboxStartedIfStarting(ctx context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error)
	// FailMCPSandboxIfUp moves a starting or ready row to failed with cause.
	FailMCPSandboxIfUp(ctx context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error)
	// MarkMCPSandboxReadyIfLive moves a row to ready after a restart or the settle, only while the run is
	// RUNNING and not lost and the row is not stopped.
	MarkMCPSandboxReadyIfLive(ctx context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error)
	// ReopenMCPSandboxIfLive moves a row back to starting for a revive's re-create, only while it is as
	// the revive read it and the run is RUNNING and not lost.
	ReopenMCPSandboxIfLive(ctx context.Context, runID uuid.UUID, ordinal int, readAt time.Time, jti string, prevUntil time.Time) (bool, error)
	// KeepMCPSandboxStartingIfLive records a revive's started sandbox on its starting row, only while the
	// row is starting and the run is RUNNING and not lost.
	KeepMCPSandboxStartingIfLive(ctx context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error)
	// FailMCPSandboxIfStarting moves a row that is still starting to failed with cause.
	FailMCPSandboxIfStarting(ctx context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error)
	// StopMCPSandboxRow moves a row in any state but stopped to stopped with cause.
	StopMCPSandboxRow(ctx context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error)
	// RotateMCPSandboxToken is a token renew's rotate: only while the row's token is oldJTI and the row is
	// not stopped.
	RotateMCPSandboxToken(ctx context.Context, runID uuid.UUID, ordinal int, oldJTI, newJTI string, oldUntil time.Time) (bool, error)
	// RecordMCPSandboxCreated writes the pinned relay address, the policy digest and the sealed proxy config
	// of a row's created sandbox, only while the row is starting or restarting.
	RecordMCPSandboxCreated(ctx context.Context, runID uuid.UUID, ordinal int, c MCPSandboxCreated) (bool, error)
	// RestartMCPSandboxIfLive moves a row that is not starting to restarting for the watcher's or a person's
	// restart, with the fresh token and the restart window, only while it is as the restart read it and the
	// run is RUNNING and not lost.
	RestartMCPSandboxIfLive(ctx context.Context, runID uuid.UUID, ordinal int, r MCPSandboxRestart) (bool, error)
}

// MCPSandboxCreated is what a created MCP sandbox's row records.
type MCPSandboxCreated struct {
	RelayAddr         string
	PolicyDigest      string
	ProxyConfigSealed []byte
}

// MCPSandboxRestart is one restart's row write: the row as the restart read it (ReadAt), the fresh token
// (JTI; the old one is served until PrevUntil), and the window after this restart.
type MCPSandboxRestart struct {
	ReadAt      time.Time
	JTI         string
	PrevUntil   time.Time
	Restarts    int
	WindowStart time.Time
}

// Compile-time assertion: PG satisfies RunMCPSandboxStore.
var _ RunMCPSandboxStore = PG{}

// InsertRunMCPSandbox writes dispatch's row for one server, as starting.
func (s PG) InsertRunMCPSandbox(ctx context.Context, row RunMCPSandbox) error {
	reserved, err := json.Marshal(row.Reserved)
	if err != nil {
		return fmt.Errorf("store: encode an MCP sandbox's reservation: %w", err)
	}
	grants := row.GrantIDs
	if grants == nil {
		grants = []uuid.UUID{}
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO run_mcp_sandboxes
		(run_id, ordinal, server_key, state, policy_digest, reserved, proxy_config_sealed, relay_addr,
		 relay_token_sha256, cp_token_jti, grant_ids)
		VALUES ($1, $2, $3, 'starting', $4, $5, $6, $7, $8, $9, $10)`,
		row.RunID, row.Ordinal, row.ServerKey, row.PolicyDigest, reserved, row.ProxyConfigSealed, row.RelayAddr,
		row.RelayTokenSHA256, row.CPTokenJTI, grants); err != nil {
		return fmt.Errorf("store: insert an MCP sandbox: %w", err)
	}
	return nil
}

// ListRunMCPSandboxes is the run's rows in ordinal order.
func (s PG) ListRunMCPSandboxes(ctx context.Context, runID uuid.UUID) ([]RunMCPSandbox, error) {
	rows, err := s.Pool.Query(ctx, `SELECT run_id, ordinal, server_key, sandbox_ref, state, restarts,
		restart_window_start, COALESCE(last_cause, ''), policy_digest, reserved, proxy_config_sealed, relay_addr,
		relay_token_sha256, cp_token_jti, COALESCE(prev_cp_token_jti, ''), prev_cp_token_until, grant_ids,
		stopped_at, created_at, updated_at
		FROM run_mcp_sandboxes WHERE run_id = $1 ORDER BY ordinal`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list a run's MCP sandboxes: %w", err)
	}
	defer rows.Close()
	var out []RunMCPSandbox
	for rows.Next() {
		var m RunMCPSandbox
		var reserved []byte
		if err := rows.Scan(&m.RunID, &m.Ordinal, &m.ServerKey, &m.SandboxRef, &m.State, &m.Restarts,
			&m.RestartWindowStart, &m.LastCause, &m.PolicyDigest, &reserved, &m.ProxyConfigSealed, &m.RelayAddr,
			&m.RelayTokenSHA256, &m.CPTokenJTI, &m.PrevCPTokenJTI, &m.PrevCPTokenUntil, &m.GrantIDs,
			&m.StoppedAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: read an MCP sandbox: %w", err)
		}
		if len(reserved) > 0 {
			if err := json.Unmarshal(reserved, &m.Reserved); err != nil {
				return nil, fmt.Errorf("store: decode an MCP sandbox's reservation: %w", err)
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list a run's MCP sandboxes: %w", err)
	}
	return out, nil
}

// mcpSandboxWrite runs one row write and reports whether it applied.
func (s PG) mcpSandboxWrite(ctx context.Context, what, sql string, args ...any) (bool, error) {
	tag, err := s.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return false, fmt.Errorf("store: %s: %w", what, err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkMCPSandboxStartedIfStarting moves one started row of runID from starting to ready, only while the run
// is STARTING. false means the run moved on first (a refused admission, a kill), and dispatch kills what it
// started.
func (s PG) MarkMCPSandboxStartedIfStarting(ctx context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error) {
	return s.mcpSandboxWrite(ctx, "mark an MCP sandbox started", `UPDATE run_mcp_sandboxes m SET state = 'ready', sandbox_ref = $3, updated_at = now()
		FROM agent_runs r
		WHERE m.run_id = $1 AND m.ordinal = $2 AND m.state = 'starting'
		  AND r.id = m.run_id AND r.state = $4`,
		runID, ordinal, ref, string(types.RunStarting))
}

// FailMCPSandboxIfUp moves one row of runID that is starting or ready to failed with cause; a stopped row
// keeps its cause.
func (s PG) FailMCPSandboxIfUp(ctx context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error) {
	return s.mcpSandboxWrite(ctx, "fail an MCP sandbox", `UPDATE run_mcp_sandboxes SET state = 'failed', last_cause = $3, updated_at = now()
		WHERE run_id = $1 AND ordinal = $2 AND state IN ('starting', 'ready')`,
		runID, ordinal, cause)
}

// MarkMCPSandboxReadyIfLive moves one MCP sandbox row of runID to ready after a restart started it, only while
// the run is RUNNING and not lost and the row is not stopped. false means the run or the row moved on first,
// and the caller kills what it started.
func (s PG) MarkMCPSandboxReadyIfLive(ctx context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE run_mcp_sandboxes m SET state = 'ready', sandbox_ref = $3, updated_at = now()
		FROM agent_runs r
		WHERE m.run_id = $1 AND m.ordinal = $2 AND m.state <> 'stopped'
		  AND r.id = m.run_id AND r.state = $4 AND r.lost_at IS NULL`,
		runID, ordinal, ref, string(types.RunRunning))
	if err != nil {
		return false, fmt.Errorf("store: mark an MCP sandbox ready: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReopenMCPSandboxIfLive moves one MCP sandbox row of runID back to starting for a revive's re-create, with
// the fresh token jti, only while the row is as the revive read it (readAt) and the run is RUNNING and not
// lost. false means the run or the row moved on first (a kill, a loss, an end, a person's Stop), and the
// revive creates nothing for that row.
func (s PG) ReopenMCPSandboxIfLive(ctx context.Context, runID uuid.UUID, ordinal int, readAt time.Time, jti string, prevUntil time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE run_mcp_sandboxes m SET state = 'starting', sandbox_ref = '',
		prev_cp_token_jti = m.cp_token_jti, prev_cp_token_until = $5, cp_token_jti = $4,
		restarts = 0, restart_window_start = now(), last_cause = NULL, stopped_at = NULL, updated_at = now()
		FROM agent_runs r
		WHERE m.run_id = $1 AND m.ordinal = $2 AND m.updated_at = $3
		  AND m.state IN ('starting', 'ready', 'restarting', 'stopped', 'failed')
		  AND r.id = m.run_id AND r.state = $6 AND r.lost_at IS NULL`,
		runID, ordinal, readAt, jti, prevUntil, string(types.RunRunning))
	if err != nil {
		return false, fmt.Errorf("store: reopen an MCP sandbox: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// KeepMCPSandboxStartingIfLive writes a revive's started sandbox ref on one row of runID, which stays
// starting, only while the row is still starting and the run is RUNNING and not lost. false means another
// writer moved the row or the run, and the revive kills what it started.
func (s PG) KeepMCPSandboxStartingIfLive(ctx context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error) {
	return s.mcpSandboxWrite(ctx, "keep an MCP sandbox starting", `UPDATE run_mcp_sandboxes m SET sandbox_ref = $3, updated_at = now()
		FROM agent_runs r
		WHERE m.run_id = $1 AND m.ordinal = $2 AND m.state = 'starting'
		  AND r.id = m.run_id AND r.state = $4 AND r.lost_at IS NULL`,
		runID, ordinal, ref, string(types.RunRunning))
}

// FailMCPSandboxIfStarting moves one row of runID that is still starting to failed with cause; a row
// another writer moved (a stopped one) keeps its state.
func (s PG) FailMCPSandboxIfStarting(ctx context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error) {
	return s.mcpSandboxWrite(ctx, "fail a starting MCP sandbox", `UPDATE run_mcp_sandboxes SET state = 'failed', last_cause = $3, updated_at = now()
		WHERE run_id = $1 AND ordinal = $2 AND state = 'starting'`,
		runID, ordinal, cause)
}

// StopMCPSandboxRow moves one row of runID in any state but stopped to stopped with cause. A row already
// stopped keeps its cause and its stopped_at, so the write is idempotent.
func (s PG) StopMCPSandboxRow(ctx context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error) {
	return s.mcpSandboxWrite(ctx, "stop an MCP sandbox", `UPDATE run_mcp_sandboxes SET state = 'stopped', last_cause = $3,
		stopped_at = now(), updated_at = now()
		WHERE run_id = $1 AND ordinal = $2 AND state <> 'stopped'`,
		runID, ordinal, cause)
}

// RotateMCPSandboxToken moves one row of runID from token oldJTI to newJTI, keeping oldJTI served until
// oldUntil (its expiry), only while the row's token is oldJTI and the row is not stopped. It changes the
// three token columns only, never updated_at, so a renew does not make a revive's reopen false.
func (s PG) RotateMCPSandboxToken(ctx context.Context, runID uuid.UUID, ordinal int, oldJTI, newJTI string, oldUntil time.Time) (bool, error) {
	return s.mcpSandboxWrite(ctx, "rotate an MCP sandbox's token", `UPDATE run_mcp_sandboxes
		SET prev_cp_token_jti = cp_token_jti, prev_cp_token_until = $5, cp_token_jti = $4
		WHERE run_id = $1 AND ordinal = $2 AND cp_token_jti = $3 AND state <> 'stopped'`,
		runID, ordinal, oldJTI, newJTI, oldUntil)
}

// RecordMCPSandboxCreated writes a created sandbox's pinned relay address, its policy digest and its sealed
// proxy config on one row of runID that is starting or restarting. false means another writer moved the row
// first (an end stopped it), and the caller kills what it created.
func (s PG) RecordMCPSandboxCreated(ctx context.Context, runID uuid.UUID, ordinal int, c MCPSandboxCreated) (bool, error) {
	return s.mcpSandboxWrite(ctx, "record a created MCP sandbox", `UPDATE run_mcp_sandboxes SET relay_addr = $3,
		policy_digest = $4, proxy_config_sealed = $5, updated_at = now()
		WHERE run_id = $1 AND ordinal = $2 AND state IN ('starting', 'restarting')`,
		runID, ordinal, c.RelayAddr, c.PolicyDigest, c.ProxyConfigSealed)
}

// RestartMCPSandboxIfLive moves one row of runID to restarting for a restart, with the fresh token jti (the
// old one served until r.PrevUntil) and the restart window r.Restarts and r.WindowStart, only while the row
// is as the restart read it (r.ReadAt) and not starting (a person may restart a stopped row; the watcher
// restarts only a ready one), and the run is RUNNING and not lost. false means the run or the row moved on
// first, and the restart creates nothing.
func (s PG) RestartMCPSandboxIfLive(ctx context.Context, runID uuid.UUID, ordinal int, r MCPSandboxRestart) (bool, error) {
	return s.mcpSandboxWrite(ctx, "restart an MCP sandbox", `UPDATE run_mcp_sandboxes m SET state = 'restarting', sandbox_ref = '',
		prev_cp_token_jti = m.cp_token_jti, prev_cp_token_until = $5, cp_token_jti = $4,
		restarts = $6, restart_window_start = $7, last_cause = NULL, stopped_at = NULL, updated_at = now()
		FROM agent_runs r
		WHERE m.run_id = $1 AND m.ordinal = $2 AND m.updated_at = $3
		  AND m.state IN ('ready', 'restarting', 'failed', 'stopped')
		  AND r.id = m.run_id AND r.state = $8 AND r.lost_at IS NULL`,
		runID, ordinal, r.ReadAt, r.JTI, r.PrevUntil, r.Restarts, r.WindowStart, string(types.RunRunning))
}
