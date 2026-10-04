// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lazyPool builds a pool that never dials (pgxpool connects on first use), so
// the boot refusal is testable without a database.
func lazyPool(t *testing.T, maxConns int) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://wardyn:wardyn@127.0.0.1:1/wardyn")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = int32(maxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestClaimSingleInstance_HARefusesSmallPool(t *testing.T) {
	_, err := claimSingleInstance(context.Background(), lazyPool(t, 2), true)
	if err == nil {
		t.Fatal("WARDYN_HA booted on pool_max_conns=2: every replica would sweep as an unfenced solo leader")
	}
	for _, want := range []string{"pool_max_conns=2", "at least 3", "WARDYN_PG_DSN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal never says %q:\n%s", want, err.Error())
		}
	}
}

func TestClaimSingleInstance_HAAcceptsElectablePool(t *testing.T) {
	release, err := claimSingleInstance(context.Background(), lazyPool(t, 3), true)
	if err != nil {
		t.Fatalf("WARDYN_HA was refused on pool_max_conns=3: %v", err)
	}
	release()
}
