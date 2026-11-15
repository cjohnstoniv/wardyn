// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for a folder record's give-up read without decoding the record. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
)

// A folder record that no longer decodes still answers its give-up from the stored JSON: when it was given up and
// its count, whether the give-up statement wrote it or the record was written so. A record not given up, a run with
// no record, and a count that is not a whole number answer nil; an unknown run is ErrNotFound. Catches: a give-up
// that only a decoded record can show, which leaves a record that never reads with no fate on the run page.
func TestPG_RunLocalFolderGivenUpReadsARecordThatDoesNotDecode(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	undecodable := func(id uuid.UUID) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = jsonb_set(local_folder_status, '{back,at}', '5') WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.RunLocalFolderStatus(ctx, id); err == nil {
			t.Fatal("the planted record decoded; want a record that does not")
		}
	}

	// Given up by the statement, then made undecodable.
	byStatement := newGiveUpRun(t, pool, endedLongAgo())
	back := givenUpBack(3)
	if out, err := pg.GiveUpRunLocalFolder(ctx, byStatement, store.GiveUpParams{Back: back, Cap: giveUpCap}); err != nil || !out.Applied {
		t.Fatalf("give-up = %+v %v, want applied", out, err)
	}
	undecodable(byStatement)
	if g, err := pg.RunLocalFolderGivenUp(ctx, byStatement); err != nil || g.At == nil || !g.At.Equal(*back.GivenUpAt) || g.Count == nil || *g.Count != 3 {
		t.Errorf("give-up read = %+v %v, want given up at %v with 3 changes", g, err, back.GivenUpAt)
	}

	// Written with no count, and with a count that is not a whole number.
	at := time.Now().UTC().Truncate(time.Microsecond)
	for name, count := range map[string]string{"no count": "null", "a count that is not a whole number": `"many"`} {
		id := newGiveUpRun(t, pool, endedLongAgo())
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = jsonb_build_object('gen', 9,
			'back', jsonb_build_object('at', 5, 'given_up_at', $2::text, 'given_up_count', $3::jsonb)) WHERE id = $1`,
			id, at.Format(time.RFC3339Nano), count); err != nil {
			t.Fatal(err)
		}
		if g, err := pg.RunLocalFolderGivenUp(ctx, id); err != nil || g.At == nil || !g.At.Equal(at) || g.Count != nil {
			t.Errorf("%s: give-up read = %+v %v, want given up at %v with no count", name, g, err, at)
		}
	}

	// Not given up, and no record at all.
	notGivenUp := newGiveUpRun(t, pool, endedLongAgo())
	undecodable(notGivenUp)
	noRecord := endedLongAgo()
	noRecord.status = nil
	for name, id := range map[string]uuid.UUID{"not given up": notGivenUp, "no record": newGiveUpRun(t, pool, noRecord)} {
		if g, err := pg.RunLocalFolderGivenUp(ctx, id); err != nil || g.At != nil || g.Count != nil {
			t.Errorf("%s: give-up read = %+v %v, want nothing given up", name, g, err)
		}
	}

	if _, err := pg.RunLocalFolderGivenUp(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown run: err = %v, want ErrNotFound", err)
	}
}
