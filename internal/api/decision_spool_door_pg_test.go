// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// downChain is a chain whose store can be taken away: while down, every batch is refused.
type downChain struct {
	*pgBatchChain
	down atomic.Bool
}

func (c *downChain) Record(ctx context.Context, ev types.AuditEvent) error {
	return c.RecordBatch(ctx, []types.AuditEvent{ev})
}

func (c *downChain) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	if c.down.Load() {
		return errors.New("store down")
	}
	return c.pgBatchChain.RecordBatch(ctx, evs)
}

// replayDoor is a decision door over chain for a run created at created, and a token for the run.
func replayDoor(t *testing.T, chain *downChain, created time.Time) (*Server, string) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	srv, _ := newBatchServer(h, &batchDoorStore{run: types.AgentRun{CreatedAt: created}})
	srv.cfg.Audit = chain
	return srv, h.mintRunToken(t, runID)
}

func postReplay(t *testing.T, srv *Server, tok, body string) int {
	t.Helper()
	hdr := batchHeaders(false)
	hdr[proxy.DecisionReplayHeader] = "1"
	return doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, body, hdr).Code
}

// TestBatchDecisions_ReplayWindow_PG: a replay keeps a record's own time back 24 hours or to the run's start,
// whichever is later, and dedupes there; a post without the mark, or a record older than that, takes the door's
// time and does not dedupe. Nothing is refused.
func TestBatchDecisions_ReplayWindow_PG(t *testing.T) {
	pool := throwawayBarePGPool(t)
	chain := &downChain{pgBatchChain: &pgBatchChain{pool: pool}}
	now := time.Now().UTC()
	srv, tok := replayDoor(t, chain, now.Add(-30*time.Hour))
	allow := retryCases()[0]
	rowTime := func(id uuid.UUID) (time.Time, int) {
		var at time.Time
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT coalesce(min(time), 'epoch'), count(*) FROM audit_events WHERE id = $1`, id).Scan(&at, &n); err != nil {
			t.Fatal(err)
		}
		return at, n
	}

	// Two hours old, with the mark: kept as is, and a second post adds nothing.
	old := now.Add(-2 * time.Hour).Truncate(time.Microsecond)
	body, ids := batchBody(t, old, allow)
	for range 2 {
		if code := postReplay(t, srv, tok, body); code != http.StatusAccepted {
			t.Fatalf("replay of a 2 h old record = %d", code)
		}
	}
	if at, n := rowTime(ids[0]); !at.Equal(old) || n != 1 {
		t.Fatalf("replayed row: time %v rows %d, want %v and 1", at, n, old)
	}

	// The same age without the mark: the door's time, and no dedupe.
	body, ids = batchBody(t, old, allow)
	before := time.Now().UTC().Add(-time.Second)
	for range 2 {
		if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
			t.Fatalf("post of a 2 h old record = %d", code)
		}
	}
	if at, n := rowTime(ids[0]); at.Before(before) || n != 2 {
		t.Fatalf("unmarked row: time %v rows %d, want the arrival time and 2", at, n)
	}

	// Older than 24 hours, though the run is older still: the door's time and no dedupe.
	body, ids = batchBody(t, now.Add(-25*time.Hour), allow)
	for range 2 {
		if code := postReplay(t, srv, tok, body); code != http.StatusAccepted {
			t.Fatalf("replay of a 25 h old record = %d", code)
		}
	}
	if at, n := rowTime(ids[0]); at.Before(before) || n != 2 {
		t.Fatalf("25 h old row: time %v rows %d, want the arrival time and 2", at, n)
	}

	// Older than the run's start: the same.
	young, tok2 := replayDoor(t, chain, now.Add(-time.Hour))
	body, ids = batchBody(t, now.Add(-2*time.Hour), allow)
	for range 2 {
		if code := postReplay(t, young, tok2, body); code != http.StatusAccepted {
			t.Fatalf("replay of a record from before the run = %d", code)
		}
	}
	if at, n := rowTime(ids[0]); at.Before(before) || n != 2 {
		t.Fatalf("pre-run row: time %v rows %d, want the arrival time and 2", at, n)
	}
}

// TestBatchDecisions_ReplayOfLandedRowAddsNothing_PG: a replay of rows the first posts landed holds them all
// and adds none, rows of a record's scan included.
func TestBatchDecisions_ReplayOfLandedRowAddsNothing_PG(t *testing.T) {
	pool := throwawayBarePGPool(t)
	chain := &downChain{pgBatchChain: &pgBatchChain{pool: pool}}
	srv, tok := replayDoor(t, chain, time.Now().UTC().Add(-3*time.Hour))
	at := time.Now().UTC().Add(-90 * time.Minute)
	body, ids := batchBody(t, at, retryCases()...)
	all := append(append([]uuid.UUID(nil), ids...), scanRowID(ids[2]))
	if code := postReplay(t, srv, tok, body); code != http.StatusAccepted {
		t.Fatalf("first replay = %d", code)
	}
	if code := postReplay(t, srv, tok, body); code != http.StatusAccepted {
		t.Fatalf("second replay = %d", code)
	}
	for id, n := range rowsByID(t, pool, all...) {
		if n != 1 {
			t.Fatalf("id %s stored %d times, want once", id, n)
		}
	}
	if len(chain.held) != 2 || !allHeld(chain.held[1]) {
		t.Fatalf("held = %v, want the second replay to hold every row", chain.held)
	}
}

func allHeld(held []bool) bool {
	for _, h := range held {
		if !h {
			return false
		}
	}
	return len(held) > 0
}

// TestDecisionSpool_SpooledBatchWritesNoReportDrop_PG: while the door answers 503 to every attempt of a batch
// with no final mark, the run's trail gets no run.report.drop row; once the store is back, the replay lands each
// record once under its own time.
func TestDecisionSpool_SpooledBatchWritesNoReportDrop_PG(t *testing.T) {
	pool := throwawayBarePGPool(t)
	chain := &downChain{pgBatchChain: &pgBatchChain{pool: pool}}
	now := time.Now().UTC()
	srv, tok := replayDoor(t, chain, now.Add(-time.Hour))
	at := now.Add(-20 * time.Minute).Truncate(time.Microsecond)
	body, ids := batchBody(t, at, retryCases()[:2]...)
	chain.down.Store(true)
	for attempt := range 4 { // the proxy's four attempts, the last without the final mark because its spool kept the batch
		if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d = %d, want 503", attempt+1, code)
		}
	}
	chain.down.Store(false)
	var drops int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action = $1`, runReportLostAction).Scan(&drops); err != nil || drops != 0 {
		t.Fatalf("%d run.report.drop rows after a spooled batch (%v)", drops, err)
	}
	if code := postReplay(t, srv, tok, body); code != http.StatusAccepted {
		t.Fatalf("replay = %d", code)
	}
	for _, id := range ids {
		var got time.Time
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT min(time), count(*) FROM audit_events WHERE id = $1`, id).Scan(&got, &n); err != nil || n != 1 || !got.Equal(at) {
			t.Fatalf("replayed record %s: time %v rows %d (%v), want %v and 1", id, got, n, err, at)
		}
	}
}
