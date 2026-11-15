// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// These tests are guarded by WARDYN_TEST_PG: they run against a real Postgres and are skipped without one.

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A killed run's stored folder record that no longer decodes still answers whether it holds a resume, from the
// stored JSON alone: a resume that is present and not JSON null holds one, as decoding reads it. A record with no
// resume or a null one, a run with no record, and an unknown run hold none. A read that fails answers its error, never
// "none". Catches: a record that does not decode read as holding no resume, which lets a stop clear a killed run's
// sandbox ref with its changes owed and no given_up row.
func TestPG_RunLocalFolderHoldsResumeReadsARecordThatDoesNotDecode(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	killed := giveUpRunOpts{state: types.RunKilled, endedAgo: secs(giveUpCap + time.Hour), ref: "sb-1", folder: true, status: ownedRecord(7)}
	for name, c := range map[string]struct {
		plant string // the stored record, changed so that it does not decode
		held  bool
	}{
		"a resume":      {`jsonb_set(local_folder_status, '{back,at}', '5')`, true},
		"no resume":     {`jsonb_set(local_folder_status - 'resume', '{back,at}', '5')`, false},
		"a null resume": {`jsonb_set(jsonb_set(local_folder_status, '{resume}', 'null'), '{back,at}', '5')`, false},
	} {
		id := newGiveUpRun(t, pool, killed)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = `+c.plant+` WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.RunLocalFolderStatus(ctx, id); err == nil {
			t.Fatalf("%s: the planted record decoded; want a record that does not", name)
		}
		if held, err := pg.RunLocalFolderHoldsResume(ctx, id); err != nil || held != c.held {
			t.Errorf("%s: holds a resume = %v %v; want %v", name, held, err, c.held)
		}
	}
	noRecord := killed
	noRecord.status = nil
	for name, id := range map[string]uuid.UUID{"no record": newGiveUpRun(t, pool, noRecord), "an unknown run": uuid.New()} {
		if held, err := pg.RunLocalFolderHoldsResume(ctx, id); err != nil || held {
			t.Errorf("%s: holds a resume = %v %v; want none", name, held, err)
		}
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if held, err := pg.RunLocalFolderHoldsResume(done, newGiveUpRun(t, pool, killed)); err == nil || held {
		t.Errorf("a read that fails: holds a resume = %v %v; want an error and no answer", held, err)
	}
}
