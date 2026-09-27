// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A run's sealed proxy config (#1176, migration 0091): what a revive rebuilds
// the run's proxy from, so the proxy container never holds it at rest.
package store

import (
	"context"
	"errors"
	"fmt"

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
	// DeleteRunProxyConfig removes it. Idempotent.
	DeleteRunProxyConfig(ctx context.Context, id uuid.UUID) error
	// PurgeTerminalRunProxyConfigs removes every row whose run is terminal,
	// the backstop for a delete that failed, and returns how many it removed.
	PurgeTerminalRunProxyConfigs(ctx context.Context) (int64, error)
}

var _ RunProxyConfigs = PG{}

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
	return nil
}

// PurgeTerminalRunProxyConfigs — see RunProxyConfigs. The terminal test is
// types.RunState.IsTerminal itself, not a copy of its set in SQL.
func (s PG) PurgeTerminalRunProxyConfigs(ctx context.Context) (int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT c.run_id, r.state FROM run_proxy_configs c JOIN agent_runs r ON r.id = c.run_id`)
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
	return tag.RowsAffected(), nil
}
