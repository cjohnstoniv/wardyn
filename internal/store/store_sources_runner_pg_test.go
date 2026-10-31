// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A local_dir source can be bound to a person's runner (sources.runner_id). The runner is part of the
// source's identity, the binding never leaves when the runner is revoked or swept, and nothing in
// Postgres unbinds it behind the member's back: the foreign key is NO ACTION, never SET NULL or
// CASCADE. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

// uniqueOnKindLocatorRef lists every unique constraint and every unique index on sources whose key is
// exactly (kind, locator, ref), with no expression: the old identity.
func uniqueOnKindLocatorRef(t *testing.T, pool *pgxpool.Pool) (constraints, indexes []string) {
	t.Helper()
	ctx := context.Background()
	collect := func(q string) []string {
		rows, err := pool.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			out = append(out, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	constraints = collect(`
		SELECT c.conname::text FROM pg_constraint c
		WHERE c.conrelid = 'sources'::regclass AND c.contype = 'u'
		  AND (SELECT array_agg(a.attname::text ORDER BY a.attname) FROM pg_attribute a
		       WHERE a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)) = ARRAY['kind','locator','ref']
		  AND cardinality(c.conkey) = 3`)
	indexes = collect(`
		SELECT i.indexrelid::regclass::text FROM pg_index i
		WHERE i.indrelid = 'sources'::regclass AND i.indisunique AND i.indexprs IS NULL
		  AND i.indnatts = 3
		  AND (SELECT array_agg(a.attname::text ORDER BY a.attname) FROM pg_attribute a
		       WHERE a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)) = ARRAY['kind','locator','ref']`)
	return constraints, indexes
}

func upsertLocalDir(t *testing.T, st store.PG, locator string, runnerID *uuid.UUID) types.Source {
	t.Helper()
	now := time.Now().UTC()
	src, err := st.UpsertSource(context.Background(), types.Source{
		ID: uuid.New(), Kind: types.SourceLocalDir, Locator: locator, Name: "src", RunnerID: runnerID,
		Status: types.WorkspacePendingScan, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("upsert %s (runner %v): %v", locator, runnerID, err)
	}
	return src
}

func countSourcesAt(t *testing.T, pool *pgxpool.Pool, locator string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sources WHERE locator = $1`, locator).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUpsertSource_RunnerIsIdentity(t *testing.T) {
	// The old identity, before the binding's migration: name it from the catalog, never assume it, so
	// the migration's DROP CONSTRAINT IF EXISTS is known to name a constraint that exists.
	before := databaseBefore(t, "0247")
	if cons, _ := uniqueOnKindLocatorRef(t, before); len(cons) != 1 || cons[0] != "sources_kind_locator_ref_key" {
		t.Fatalf("before the binding, unique constraints on (kind, locator, ref) = %v, want [sources_kind_locator_ref_key]", cons)
	}

	pool := databaseBefore(t, "zzz") // every migration applied
	if cons, idx := uniqueOnKindLocatorRef(t, pool); len(cons) != 0 || len(idx) != 0 {
		t.Fatalf("a unique key on exactly (kind, locator, ref) remains: constraints %v, indexes %v", cons, idx)
	}
	st := store.PG{Pool: pool}
	rn := claimedRunner(t, st, "alice@example.com")
	const path = "/Users/alice/src/payments-api"

	unbound := upsertLocalDir(t, st, path, nil)
	bound := upsertLocalDir(t, st, path, &rn.ID)
	if bound.ID == unbound.ID || bound.RunnerID == nil || *bound.RunnerID != rn.ID || unbound.RunnerID != nil {
		t.Fatalf("unbound %+v and bound %+v at one path are not two sources", unbound, bound)
	}
	if n := countSourcesAt(t, pool, path); n != 2 {
		t.Fatalf("unbound + bound at one path = %d rows, want 2", n)
	}
	if again := upsertLocalDir(t, st, path, &rn.ID); again.ID != bound.ID {
		t.Fatalf("the same path and runner again = %s, want the existing %s", again.ID, bound.ID)
	}
	if again := upsertLocalDir(t, st, path, nil); again.ID != unbound.ID {
		t.Fatalf("the same unbound path again = %s, want the existing %s", again.ID, unbound.ID)
	}
	if n := countSourcesAt(t, pool, path); n != 2 {
		t.Fatalf("after upserting each again = %d rows, want 2", n)
	}
	got, err := st.GetSource(context.Background(), bound.ID)
	if err != nil || got.RunnerID == nil || *got.RunnerID != rn.ID {
		t.Fatalf("read back = %+v, %v; want runner %s", got, err, rn.ID)
	}
}

// The only hard delete of a runners row is the unclaimed sweep, and a bound runner was claimed, so the
// sweep never reaches it and never trips the binding's foreign key.
func TestExpireUnclaimedRunners_NeverDeletesBoundRunner(t *testing.T) {
	pool := databaseBefore(t, "zzz")
	st := store.PG{Pool: pool}
	ctx := context.Background()
	rn := claimedRunner(t, st, "alice@example.com")
	stray, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	src := upsertLocalDir(t, st, "/Users/alice/src/app", &rn.ID)
	n, err := st.ExpireUnclaimedRunners(ctx, time.Now().Add(100*365*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("expire = %d, %v; want the one unclaimed runner swept with no error", n, err)
	}
	if _, err := st.GetRunner(ctx, stray.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the unclaimed runner survived: %v", err)
	}
	if _, err := st.GetRunner(ctx, rn.ID); err != nil {
		t.Fatalf("the bound runner was swept: %v", err)
	}
	if got, err := st.GetSource(ctx, src.ID); err != nil || got.RunnerID == nil || *got.RunnerID != rn.ID {
		t.Fatalf("the bound source after the sweep = %+v, %v; want still bound to %s", got, err, rn.ID)
	}
}

// A direct delete of a bound runner is stopped by the binding (23503): the key is NO ACTION, so a
// laptop path never silently turns into a path on wardynd's host (SET NULL) and the member's source
// never vanishes (CASCADE).
func TestSourcesRunnerFK_NoAction(t *testing.T) {
	pool := databaseBefore(t, "zzz")
	st := store.PG{Pool: pool}
	ctx := context.Background()
	var action string
	if err := pool.QueryRow(ctx, `
		SELECT c.confdeltype::text FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
		WHERE c.conrelid = 'sources'::regclass AND c.contype = 'f' AND a.attname = 'runner_id'
		  AND c.confrelid = 'runners'::regclass`).Scan(&action); err != nil {
		t.Fatalf("sources.runner_id has no foreign key to runners: %v", err)
	}
	if action != "a" {
		t.Fatalf("sources.runner_id ON DELETE = %q, want NO ACTION (a)", action)
	}
	rn := claimedRunner(t, st, "alice@example.com")
	src := upsertLocalDir(t, st, "/Users/alice/src/app", &rn.ID)
	_, err := pool.Exec(ctx, `DELETE FROM runners WHERE id = $1`, rn.ID)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23503" {
		t.Fatalf("deleting a bound runner = %v, want SQLSTATE 23503", err)
	}
	if got, err := st.GetSource(ctx, src.ID); err != nil || got.RunnerID == nil || *got.RunnerID != rn.ID {
		t.Fatalf("the bound source after the refused delete = %+v, %v; want still bound to %s", got, err, rn.ID)
	}
}
