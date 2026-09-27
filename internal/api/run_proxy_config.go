// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A run's rendered proxy config at rest (#1176). It carries the run token, the
// per-run MITM CA key and the operator's upstream-proxy credential, so it is
// kept in exactly one place: sealed in the run_proxy_configs row (migration
// 0090), under Config.RunConfigKey, bound to its run. The proxy gets it over
// stdin and holds it only in memory; a revive rebuilds the proxy from this row
// and never reads the proxy container, which the driver removes when the run
// is kept. The row is deleted when the run goes terminal (revokeRunCascade,
// killTeardownTail), with purgeTerminalRunProxyConfigs as the backstop.

// runProxyConfigAADLabel is the domain label of the row's associated data, so
// a sealed config can open only as the config of the run it was sealed for.
const runProxyConfigAADLabel = "wardyn/run-proxy-config/v1"

// errRunProxyConfigNotKept: this deployment keeps no run proxy config (no key,
// or a store without the table: test doubles and store-less embeddings).
var errRunProxyConfigNotKept = errors.New("this deployment does not keep run proxy configs")

func runProxyConfigAAD(runID uuid.UUID) []byte {
	return kek.Encode(runProxyConfigAADLabel, runID.String())
}

func (s *Server) runProxyConfigStore() (store.RunProxyConfigs, bool) {
	st, ok := s.cfg.Store.(store.RunProxyConfigs)
	return st, ok && len(s.cfg.RunConfigKey) == kek.DEKSize
}

// keepRunProxyConfig renders pc as the proxy will receive it and stores it
// sealed. A deployment that keeps none stores nothing and says so with nil:
// the run still starts, and a revive of it is refused.
func (s *Server) keepRunProxyConfig(ctx context.Context, runID uuid.UUID, pc runner.ProxyConfig) error {
	cfgJSON, err := runner.BuildProxyConfig(runID, pc, runner.ProxyListenPort)
	if err != nil {
		return fmt.Errorf("render proxy config: %w", err)
	}
	return s.storeRunProxyConfig(ctx, runID, cfgJSON)
}

// storeRunProxyConfig seals cfgJSON for runID and writes it, replacing any.
func (s *Server) storeRunProxyConfig(ctx context.Context, runID uuid.UUID, cfgJSON []byte) error {
	st, ok := s.runProxyConfigStore()
	if !ok {
		return nil
	}
	sealed, err := kek.Seal(s.cfg.RunConfigKey, cfgJSON, runProxyConfigAAD(runID))
	if err != nil {
		return fmt.Errorf("seal proxy config: %w", err)
	}
	return st.PutRunProxyConfig(ctx, runID, sealed)
}

// loadRunProxyConfig reads and opens runID's config: store.ErrNotFound when
// it has none, errRunProxyConfigNotKept when this deployment keeps none.
func (s *Server) loadRunProxyConfig(ctx context.Context, runID uuid.UUID) ([]byte, error) {
	st, ok := s.runProxyConfigStore()
	if !ok {
		return nil, errRunProxyConfigNotKept
	}
	sealed, err := st.GetRunProxyConfig(ctx, runID)
	if err != nil {
		return nil, err
	}
	cfgJSON, err := kek.Open(s.cfg.RunConfigKey, sealed, runProxyConfigAAD(runID))
	if err != nil {
		return nil, fmt.Errorf("open proxy config: %w", err)
	}
	return cfgJSON, nil
}

// dropRunProxyConfig deletes runID's config, for a run that has gone terminal.
// A failure is returned for the caller's audit row; the purge retries it.
func (s *Server) dropRunProxyConfig(ctx context.Context, runID uuid.UUID) error {
	st, ok := s.cfg.Store.(store.RunProxyConfigs)
	if !ok {
		return nil
	}
	return st.DeleteRunProxyConfig(ctx, runID)
}

// purgeTerminalRunProxyConfigs deletes the config of every terminal run: the
// backstop for a drop that failed, run at boot and on the orphan sweep's
// cadence.
func (s *Server) purgeTerminalRunProxyConfigs(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.RunProxyConfigs)
	if !ok {
		return nil
	}
	n, err := st.PurgeTerminalRunProxyConfigs(ctx)
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: purged terminal runs' proxy configs", slog.Int64("purged", n))
	}
	return err
}
