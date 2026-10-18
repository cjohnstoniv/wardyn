// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// Store-mode writes whose COMMIT outcome is unknown, and pointer rows the
// configured store no longer derives. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"testing"
	"time"
)

// A store-mode Put whose COMMIT reaches the server while its reply is lost (the
// caller's deadline ends mid-commit) keeps the value its committed row points
// to, and reports the save (#2812).
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
	if err := s.For("bob").Put(ctx, "slow", []byte("the-value")); err != nil {
		t.Fatalf("Put whose commit landed = %v; want the save reported", err)
	}
	if n := ext.deletes(); n != 0 {
		t.Fatalf("%d external deletes; want none — the committed row points to the value", n)
	}
	mustGetOwned(t, s, "bob", "slow", "the-value")
}

