// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// CheckRows, the read-only opener behind `wardynd -recovery-rehearse`: it opens every row over one
// connection that cannot write, reads pre-envelope rows without converting them, never reads an
// external store, and carries no value in a result.

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// countingExt counts the reads an external store receives.
type countingExt struct {
	*memExt
	gets atomic.Int32
}

func (c *countingExt) Get(ctx context.Context, owner, name, ref string) ([]byte, error) {
	c.gets.Add(1)
	return c.memExt.Get(ctx, owner, name, ref)
}

// readOnlyConn is a connection whose every statement is read-only, as the rehearsal's target backend is.
func readOnlyConn(t *testing.T, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	// The connection comes from a pool of its own: the test goes on to read through pool while it is held.
	holder, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(holder.Close)
	conn, err := holder.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `SET default_transaction_read_only = on`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SET default_transaction_read_only = off`)
		conn.Release()
	})
	return conn
}

func checkAll(t *testing.T, s *Store, q RowReader, legacy age.Identity) map[string]CheckedRow {
	t.Helper()
	got := map[string]CheckedRow{}
	if err := s.CheckRows(t.Context(), q, legacy, func(c CheckedRow) {
		if c.PrincipalKey {
			got["key:"+c.Owner+"/"+c.Name] = c
			return
		}
		got[rowRef(c.Owner, c.Name)] = c
	}); err != nil {
		t.Fatalf("CheckRows: %v", err)
	}
	return got
}

type tableState struct {
	secrets map[string]rawRow
	keys    int
}

func snapshotTables(t *testing.T, pool *pgxpool.Pool) tableState {
	t.Helper()
	return tableState{rawRows(t, pool), count(t, pool, `SELECT count(*) FROM principal_keys`)}
}

func TestCheckRows_RightKeyOpensEveryRowAndWritesNothing(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	vault := &countingExt{memExt: newMemExt("vaultkv")}
	seedMixed(t, pool, id, vault.memExt, newMemExt("azurekv"))
	seedV0(t, pool, id, "", "legacy-credential", "legacy-canary-value")
	before := snapshotTables(t, pool)
	principal := count(t, pool, `SELECT count(*) FROM principal_keys WHERE destroyed_at IS NULL`)

	s := mixedStore(t, pool, id, false, vault, false)
	got := checkAll(t, s, readOnlyConn(t, pool), id)

	want := map[string]RowOutcome{
		rowRef("", "wardyn-signing-key"): RowOpened,
		rowRef("", "operator-token"):     RowOpened,
		rowRef("bob", "v1-cred"):         RowOpened,
		rowRef("bob", "pk-b"):            RowOpened,
		rowRef("alice", "pk-a"):          RowOpened,
		rowRef("bob", "vault-cred"):      RowExternal,
		rowRef("zoe", "azure-cred"):      RowExternal,
		rowRef("", "legacy-credential"):  RowConverts,
		"key:alice/" + "cred":            RowOpened,
		"key:bob/" + "cred":              RowOpened,
	}
	if len(got) != len(want) {
		t.Errorf("CheckRows reported %d results, want %d: %v", len(got), len(want), got)
	}
	for ref, outcome := range want {
		if g := got[ref]; g.Outcome != outcome || g.Err != nil {
			t.Errorf("%s = %q (err %v), want %q", ref, g.Outcome, g.Err, outcome)
		}
	}
	if principal != 2 {
		t.Fatalf("fixture drift: %d live principal keys, want 2", principal)
	}
	if n := vault.gets.Load(); n != 0 {
		t.Errorf("the external store received %d reads; a pointer row must never be read", n)
	}
	after := snapshotTables(t, pool)
	if after.keys != before.keys || len(after.secrets) != len(before.secrets) {
		t.Fatalf("row counts changed: before %d/%d, after %d/%d", len(before.secrets), before.keys, len(after.secrets), after.keys)
	}
	for ref, r := range before.secrets {
		a := after.secrets[ref]
		if a.version != r.version || a.kekID != r.kekID || !bytes.Equal(a.wrapped, r.wrapped) || !bytes.Equal(a.ct, r.ct) {
			t.Errorf("%s changed: the check must leave a pre-envelope row as it found it", ref)
		}
	}
}

func TestCheckRows_AWrongAgeKeyFailsEveryLocalRowByReference(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	seedMixed(t, pool, id, newMemExt("vaultkv"), newMemExt("azurekv"))
	seedV0(t, pool, id, "", "legacy-credential", "legacy-canary-value")

	other := mustIdentity(t)
	got := checkAll(t, mixedStore(t, pool, other, false, nil, false), readOnlyConn(t, pool), other)
	for _, ref := range []string{rowRef("", "operator-token"), rowRef("bob", "v1-cred"), rowRef("", "legacy-credential"), rowRef("alice", "pk-a"), "key:alice/cred"} {
		g := got[ref]
		if g.Outcome != RowFailed || g.Err == nil {
			t.Errorf("%s under a wrong age key = %q, want %q", ref, g.Outcome, RowFailed)
			continue
		}
		if g.Err != nil && strings.Contains(g.Err.Error(), "canary") {
			t.Errorf("%s: the error carries a value: %v", ref, g.Err)
		}
	}
	if g := got[rowRef("bob", "vault-cred")]; g.Outcome != RowExternal {
		t.Errorf("a pointer row = %q under a wrong key; it is never opened", g.Outcome)
	}
}

func TestCheckRows_ADestroyedPrincipalKeyIsDestroyedNotFailed(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	on := mixedStore(t, pool, id, true, nil, false)
	mustPut(t, on, "bob", "pk-1", "bob-v3-1")
	var wrapped, ct []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_dek, ciphertext FROM secrets WHERE owned_by='bob' AND name='pk-1'`).Scan(&wrapped, &ct); err != nil {
		t.Fatal(err)
	}
	if _, err := secretstore.EraseOwner(t.Context(), on, "bob"); err != nil {
		t.Fatal(err)
	}
	// The row as a backup taken before the erasure holds it.
	if _, err := pool.Exec(t.Context(), `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ('bob', 'pk-1', 3, 'pk:v1', $1, $2)`, wrapped, ct); err != nil {
		t.Fatal(err)
	}
	got := checkAll(t, on, readOnlyConn(t, pool), id)
	if g := got[rowRef("bob", "pk-1")]; g.Outcome != RowDestroyed || g.Err != nil {
		t.Errorf("a row under an erased key = %q (err %v), want %q", g.Outcome, g.Err, RowDestroyed)
	}
	if _, listed := got["key:bob/cred"]; listed {
		t.Error("a destroyed principal key is listed as live")
	}
}
