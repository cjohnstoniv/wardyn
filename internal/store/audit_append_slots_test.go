// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A pool that has dialled nothing is enough: the slot count reads only its configured size.
func TestAuditAppendSlotsSizeFromThePool(t *testing.T) {
	for _, tc := range []struct {
		maxConns int32
		want     int
	}{{1, 1}, {2, 1}, {8, 4}, {20, 4}} {
		cfg, err := pgxpool.ParseConfig("postgres://wardyn@127.0.0.1:1/none")
		if err != nil {
			t.Fatal(err)
		}
		cfg.MaxConns = tc.maxConns
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := store.AuditAppendSlotCount(pool); got != tc.want {
			t.Errorf("MaxConns %d: %d slots, want %d", tc.maxConns, got, tc.want)
		}
		pool.Close()
	}
}
