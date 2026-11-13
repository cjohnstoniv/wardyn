// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// miscCovOrphanStore is miscCovOutStore with the record of live chunks that have no run_outputs row
// (store.RunOutputOrphanStore). Only the sweep calls it, on the test's goroutine.
type miscCovOrphanStore struct {
	*miscCovOutStore
	endedErr  error
	endedN    int
	endedAges []time.Duration
	orphans   []uuid.UUID
	orphanErr error
	listAges  []time.Duration
	insertErr error
}

func (s *miscCovOrphanStore) DeleteEndedRunOutputChunks(_ context.Context, age time.Duration) (int, error) {
	s.endedAges = append(s.endedAges, age)
	return s.endedN, s.endedErr
}

func (s *miscCovOrphanStore) ListOrphanRunOutputChunks(_ context.Context, age time.Duration, _ int) ([]uuid.UUID, error) {
	s.listAges = append(s.listAges, age)
	return s.orphans, s.orphanErr
}

func (s *miscCovOrphanStore) InsertPendingRunOutput(ctx context.Context, id uuid.UUID) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	return s.miscCovOutStore.InsertPendingRunOutput(ctx, id)
}

// miscCovOrphanFixture is miscCovOutFixture over the orphan record, its run COMPLETED ago.
func miscCovOrphanFixture(t *testing.T, ago time.Duration, shape ...func(*Config)) (*outputFixture, *miscCovOrphanStore) {
	t.Helper()
	f, cs := miscCovOutFixture(t, shape...)
	orph := &miscCovOrphanStore{miscCovOutStore: cs}
	f.srv.cfg.Store = orph
	ended := time.Now().Add(-ago)
	f.st.mu.Lock()
	f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
	f.st.mu.Unlock()
	return f, orph
}

// A run that ended longer ago than WARDYN_RUN_OUTPUT_RETENTION_DAYS reads 410 run_output_expired with
// none of the chunks a fault left, which are not even read; inside the window they are served.
func TestMiscCovOrphanChunksExpireWithTheirRun(t *testing.T) {
	keep := func(c *Config) { c.RunOutputRetention = 30 * 24 * time.Hour }
	f, orph := miscCovOrphanFixture(t, 40*24*time.Hour, keep)
	orph.readRes = store.RunOutputChunks{Found: true, Bytes: []byte("left by a fault")}
	w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output", adminToken, "")
	if w.Code != http.StatusGone || errorReason(w) != reasonRunOutputExpired || strings.Contains(w.Body.String(), "left by a fault") {
		t.Fatalf("read past the window = %d %s, want 410 %s and none of the chunks", w.Code, w.Body, reasonRunOutputExpired)
	}
	if len(orph.reads) != 0 {
		t.Errorf("the expired run's chunks were read %d times, want none", len(orph.reads))
	}

	g, gs := miscCovOrphanFixture(t, 24*time.Hour, keep)
	gs.readRes = store.RunOutputChunks{Found: true, Bytes: []byte("left by a fault")}
	if code, got := g.get(t); code != http.StatusOK || got.Output != "left by a fault" {
		t.Errorf("read inside the window = %d %+v, want the chunks", code, got)
	}
}

// The sweep deletes the live chunks of runs that ended past the window, logging how many when there
// were any, and gives every other ended run's orphaned chunks the pending row they were owed. An erased run gets no
// row; a failed insert waits for the next sweep; a failed list is the pass's error and the stale pass
// still runs; with persistence off nothing is adopted.
func TestMiscCovSweepAgesAndAdoptsOrphanChunks(t *testing.T) {
	const retention = 72 * time.Hour
	keep := func(c *Config) { c.RunOutputRetention = retention }
	sweep := func(t *testing.T, f *outputFixture) error {
		t.Helper()
		return f.srv.SweepRunOutputs(t.Context())
	}

	const deletedLine = "wardynd: deleted the live output chunks of runs that ended past the retention window"
	t.Run("ended runs' chunks are deleted past the window", func(t *testing.T) {
		logs := captureSlog(t)
		f, orph := miscCovOrphanFixture(t, time.Hour, keep)
		if err := sweep(t, f); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(orph.endedAges, []time.Duration{retention}) {
			t.Errorf("chunks deleted past %v, want the retention window", orph.endedAges)
		}
		if strings.Contains(logs.String(), deletedLine) {
			t.Errorf("log = %q, want no deleted-chunks line when none were deleted", logs.String())
		}
	})
	t.Run("deleted chunks are logged with their count", func(t *testing.T) {
		logs := captureSlog(t)
		f, orph := miscCovOrphanFixture(t, time.Hour, keep)
		orph.endedN = 3
		if err := sweep(t, f); err != nil {
			t.Fatal(err)
		}
		if got := logs.String(); strings.Count(got, deletedLine) != 1 || !strings.Contains(got, "deleted=3") {
			t.Errorf("log = %q, want one %q line with deleted=3", got, deletedLine)
		}
	})
	t.Run("a failed chunk delete is the pass's error and the pass goes on", func(t *testing.T) {
		f, orph := miscCovOrphanFixture(t, time.Hour, keep)
		orph.endedErr = errors.New("delete refused")
		if err := sweep(t, f); !errors.Is(err, orph.endedErr) || orph.listed != 1 {
			t.Errorf("err = %v after %d stale lists, want the delete's error and the pass to go on", err, orph.listed)
		}
	})
	t.Run("an orphan gets the pending row it was owed", func(t *testing.T) {
		f, orph := miscCovOrphanFixture(t, time.Hour, keep)
		orph.orphans = []uuid.UUID{f.run.ID}
		if err := sweep(t, f); err != nil {
			t.Fatal(err)
		}
		if row, ok := f.mem.row(f.run.ID); !ok || row.CapturedAt != nil {
			t.Errorf("row = %+v found=%v, want a pending row", row, ok)
		}
		if !slices.Equal(orph.listAges, []time.Duration{retention}) {
			t.Errorf("orphans listed with %v, want the retention window", orph.listAges)
		}
	})
	t.Run("an erased run gets no row", func(t *testing.T) {
		f, orph := miscCovOrphanFixture(t, time.Hour)
		orph.orphans = []uuid.UUID{f.run.ID}
		f.mem.mu.Lock()
		f.mem.erased[f.run.ID] = true
		f.mem.mu.Unlock()
		if err := sweep(t, f); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.mem.row(f.run.ID); ok {
			t.Error("an erased run was given a row")
		}
	})
	t.Run("a failed insert waits for the next sweep", func(t *testing.T) {
		f, orph := miscCovOrphanFixture(t, time.Hour)
		orph.orphans, orph.insertErr = []uuid.UUID{f.run.ID}, errors.New("insert refused")
		if err := sweep(t, f); err != nil || orph.listed != 1 {
			t.Errorf("err = %v after %d stale lists, want nil and the pass to go on", err, orph.listed)
		}
	})
	t.Run("a failed list is the pass's error and the stale pass still runs", func(t *testing.T) {
		f, orph := miscCovOrphanFixture(t, time.Hour)
		orph.orphanErr = errors.New("list refused")
		if err := sweep(t, f); !errors.Is(err, orph.orphanErr) || orph.listed != 1 {
			t.Errorf("err = %v after %d stale lists, want the list's error and the pass to go on", err, orph.listed)
		}
	})
	t.Run("with persistence off nothing is adopted and ended chunks still go", func(t *testing.T) {
		f, orph := miscCovOrphanFixture(t, time.Hour, keep, func(c *Config) { c.RunOutputPersistOff = true })
		orph.orphans = []uuid.UUID{f.run.ID}
		if err := sweep(t, f); err != nil {
			t.Fatal(err)
		}
		if len(orph.listAges) != 0 || !slices.Equal(orph.endedAges, []time.Duration{retention}) {
			t.Errorf("listed %d times, deleted past %v; want no list and the window's delete", len(orph.listAges), orph.endedAges)
		}
	})
}
