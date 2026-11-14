// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// Store-mode writes whose COMMIT outcome is unknown, and pointer rows the
// configured store no longer derives. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A store-mode Put whose COMMIT reaches the server while its reply is lost (the
// caller's deadline ends mid-commit) keeps the value its committed row points
// to (#2812). It answers at once that whether the row was written is unknown:
// the recheck runs after it returns.
func TestPutExternal_ALostCommitReplyKeepsTheCommittedValue(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newMemExt("vaultkv")
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	// A deferred trigger holds the COMMIT past the caller's deadline, and outlasts
	// the cancel request pgx sends as it closes the connection: the commit
	// lands, its reply never arrives.
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION slow_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			BEGIN PERFORM pg_sleep(2); EXCEPTION WHEN query_canceled THEN PERFORM pg_sleep(1); END;
			RETURN NULL; END $$;
		CREATE CONSTRAINT TRIGGER slow_commit AFTER INSERT ON secrets DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW WHEN (NEW.name = 'slow') EXECUTE FUNCTION slow_commit();`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := s.For("bob").Put(ctx, "slow", []byte("the-value")); !errors.Is(err, secretstore.ErrRowNotWritten) || !strings.Contains(err.Error(), "is unknown") {
		t.Fatalf("Put whose commit reply was lost = %v; want ErrRowNotWritten saying whether the row was written is unknown", err)
	}
	WaitForCleanUps()
	if n := ext.deletes(); n != 0 {
		t.Fatalf("%d external deletes; want none — the committed row points to the value", n)
	}
	mustGetOwned(t, s, "bob", "slow", "the-value")
}

// UnderivedPointers counts the pointer rows the configured store no longer
// derives (written under another prefix or address), and only those (#2807).
func TestUnderivedPointers_CountsRowsTheStoreNoLongerDerives(t *testing.T) {
	pool := rekeyDatabase(t)
	s := mixedStore(t, pool, mustIdentity(t), false, newMemExt("vaultkv"), true)
	mustPut(t, s, "bob", "derived", "v")
	if _, err := pool.Exec(t.Context(), `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES
		('bob', 'moved', 2, 'vaultkv:old-prefix/bob/moved', '', ''),
		('zoe', 'elsewhere', 2, 'azurekv:vault.example/x-g1#1', '', '')`); err != nil {
		t.Fatal(err)
	}
	n, ref, err := s.UnderivedPointers(t.Context())
	if err != nil || n != 1 || ref != "old-prefix/bob/moved" {
		t.Fatalf("UnderivedPointers = (%d, %q, %v); want the one vaultkv row under the old prefix", n, ref, err)
	}
}
