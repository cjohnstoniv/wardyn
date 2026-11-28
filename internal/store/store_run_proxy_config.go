// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A run's sealed proxy config (#1176, migration 0093): what a revive rebuilds
// the run's proxy from, so the proxy container never holds it at rest.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunProxyConfigs stores each run's sealed proxy config. The caller seals and
// opens it; the store never sees it in the clear. Optional for RunLeaser's
// reason: the api layer type-asserts, and production is always PG.
type RunProxyConfigs interface {
	// PutRunProxyConfig writes run id's sealed config, replacing any.
	PutRunProxyConfig(ctx context.Context, id uuid.UUID, sealed []byte) error
	// GetRunProxyConfig reads it; ErrNotFound when there is none.
	GetRunProxyConfig(ctx context.Context, id uuid.UUID) ([]byte, error)
	// DeleteRunProxyConfig removes it, then the run's sealed sandbox spec (run_sandbox_specs) in a statement of its
	// own: a failed spec delete is returned wrapped in ErrRunSandboxSpecKept, and never keeps the proxy config.
	// Idempotent.
	DeleteRunProxyConfig(ctx context.Context, id uuid.UUID) error
	// PurgeTerminalRunProxyConfigs removes every row whose run is terminal,
	// the backstop for a delete that failed, and returns how many it removed.
	// It then removes every terminal run's sealed sandbox spec the same way,
	// logs both counts, and returns the proxy configs' count with either error,
	// a failed spec purge wrapped in ErrRunSandboxSpecKept.
	PurgeTerminalRunProxyConfigs(ctx context.Context) (int64, error)
}

var _ RunProxyConfigs = PG{}

// ErrRunSandboxSpecKept: the run's proxy config is deleted, but its sealed sandbox spec could not be. It is not a
// failed containment step; the purge deletes the spec once the app role may.
var ErrRunSandboxSpecKept = errors.New("store: run proxy config deleted, its sealed sandbox spec kept")

// PutRunProxyConfig — see RunProxyConfigs.
func (s PG) PutRunProxyConfig(ctx context.Context, id uuid.UUID, sealed []byte) error {
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO run_proxy_configs (run_id, sealed) VALUES ($1, $2)
		ON CONFLICT (run_id) DO UPDATE SET sealed=$2, updated_at=now()`, id, sealed); err != nil {
		return fmt.Errorf("store: put run proxy config: %w", err)
	}
	return nil
}

// GetRunProxyConfig — see RunProxyConfigs.
func (s PG) GetRunProxyConfig(ctx context.Context, id uuid.UUID) ([]byte, error) {
	var sealed []byte
	err := s.Pool.QueryRow(ctx, `SELECT sealed FROM run_proxy_configs WHERE run_id=$1`, id).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run proxy config: %w", err)
	}
	return sealed, nil
}

// DeleteRunProxyConfig — see RunProxyConfigs.
func (s PG) DeleteRunProxyConfig(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM run_proxy_configs WHERE run_id=$1`, id); err != nil {
		return fmt.Errorf("store: delete run proxy config: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM run_sandbox_specs WHERE run_id=$1`, id); err != nil {
		return fmt.Errorf("%w: %w", ErrRunSandboxSpecKept, err)
	}
	return nil
}

// PurgeTerminalRunProxyConfigs — see RunProxyConfigs. The terminal test is
// types.RunState.IsTerminal itself, not a copy of its set in SQL.
func (s PG) PurgeTerminalRunProxyConfigs(ctx context.Context) (int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT r.id, r.state FROM agent_runs r
		WHERE r.id IN (SELECT run_id FROM run_proxy_configs UNION SELECT run_id FROM run_sandbox_specs)`)
	if err != nil {
		return 0, fmt.Errorf("store: list run proxy configs: %w", err)
	}
	var terminal []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: list run proxy configs: %w", err)
		}
		if types.RunState(state).IsTerminal() {
			terminal = append(terminal, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: list run proxy configs: %w", err)
	}
	if len(terminal) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM run_proxy_configs WHERE run_id = ANY($1)`, terminal)
	if err != nil {
		return 0, fmt.Errorf("store: purge terminal run proxy configs: %w", err)
	}
	configs := tag.RowsAffected()
	if tag, err = s.Pool.Exec(ctx, `DELETE FROM run_sandbox_specs WHERE run_id = ANY($1)`, terminal); err != nil {
		slog.ErrorContext(ctx, "store: terminal runs' proxy configs purged, their sandbox specs kept", slog.Int64("proxy_configs", configs), slog.Any("error", err))
		return configs, fmt.Errorf("%w: purge terminal run sandbox specs: %w", ErrRunSandboxSpecKept, err)
	}
	slog.InfoContext(ctx, "store: purged terminal runs' proxy configs and sandbox specs", slog.Int64("proxy_configs", configs), slog.Int64("sandbox_specs", tag.RowsAffected()))
	return configs, nil
}
