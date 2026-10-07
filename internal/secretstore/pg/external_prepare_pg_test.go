// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/testutil"
)

type checkedExternal struct {
	*memExt
	t       *testing.T
	pool    *pgxpool.Pool
	observe *pgx.Conn
	calls   map[string]int
}

func (e *checkedExternal) check(ctx context.Context, operation string, held int32) {
	e.t.Helper()
	e.calls[operation]++
	if _, bounded := ctx.Deadline(); !bounded {
		e.t.Errorf("%s has no deadline", operation)
	}
	if got := e.pool.Stat().AcquiredConns(); got != held {
		e.t.Errorf("%s holds %d pooled connections, want %d", operation, got, held)
	}
	var transactions int
	err := e.observe.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE datname=current_database() AND pid<>pg_backend_pid() AND xact_start IS NOT NULL`).Scan(&transactions)
	if err != nil || transactions != 0 {
		e.t.Errorf("%s has %d database transactions open: %v", operation, transactions, err)
	}
}

func (e *checkedExternal) Put(ctx context.Context, owner, name, ref string, value []byte, createOnly bool) (string, error) {
	e.check(ctx, "put", 1)
	return e.memExt.Put(ctx, owner, name, ref, value, createOnly)
}

func (e *checkedExternal) Get(ctx context.Context, owner, name, ref string) ([]byte, error) {
	e.check(ctx, "get", 0)
	return e.memExt.Get(ctx, owner, name, ref)
}

func (e *checkedExternal) Delete(ctx context.Context, owner, name, ref string) error {
	e.check(ctx, "delete", 1)
	return e.memExt.Delete(ctx, owner, name, ref)
}

func TestPG_MigrationExternalCallsHaveNoTransaction(t *testing.T) {
	base := rekeyDatabase(t)
	pool := singleSecretPool(t, base)
	ext := &checkedExternal{memExt: newMemExt("vaultkv"), t: t, pool: pool, observe: testutil.PGConn(t, base), calls: map[string]int{}}
	s := mixedStore(t, pool, mustIdentity(t), true, ext, false)
	mustPut(t, s, "alice", "token", "value")
	for _, target := range []string{"vaultkv", MigrateLocal} {
		if got, err := s.Migrate(t.Context(), target, func(string, string) {}); err != nil || got.Moved != 1 {
			t.Fatalf("migrate to %s = %+v, %v", target, got, err)
		}
	}
	for _, op := range []string{"get", "put", "delete"} {
		if ext.calls[op] != 1 {
			t.Errorf("%s calls = %d, want 1", op, ext.calls[op])
		}
	}
	mustGetOwned(t, s, "alice", "token", "value")
}
