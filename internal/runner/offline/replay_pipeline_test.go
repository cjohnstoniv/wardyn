// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type orgReply int

const (
	orgHold orgReply = iota
	orgAck
	orgDrop
)

// replayAgainstOrg runs Manager.Replay over a real runner peer whose org end is
// driven by hand: reply decides, per received spool row, whether the org sends
// a cumulative ACK, sends nothing, or drops the link. Every ACK it writes is one
// round trip the runner waited for.
func replayAgainstOrg(t *testing.T, m *Manager, reply func(n int, row runnerspool.Record) orgReply) (rows []runnerspool.Record, acks int, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	org, run := runnerwire.Loopback()
	peer := runnerwire.NewPeer(run, runnerwire.PeerConfig{})
	defer peer.Close()
	go peer.Run(ctx) //nolint:errcheck // the test owns shutdown
	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan struct{})
	go func() {
		defer close(served)
		for {
			f, err := org.ReadFrame(serveCtx)
			if err != nil {
				return
			}
			var ev runnerwire.Event
			var row runnerspool.Record
			if f.Type != runnerwire.TypeEvent || runnerwire.Unmarshal(f.Payload, &ev) != nil || ev.Kind != runnerwire.EventSpool || json.Unmarshal(ev.Data, &row) != nil {
				t.Errorf("unexpected frame %s", f.Type)
				return
			}
			rows = append(rows, row)
			switch reply(len(rows), row) {
			case orgAck:
				if err := org.WriteFrame(serveCtx, runnerwire.Frame{Type: runnerwire.TypeAck, Payload: runnerwire.EncodeAck(f.Seq)}); err != nil {
					return
				}
				acks++
			case orgDrop:
				org.Drop()
				return
			}
		}
	}()
	err = m.Replay(ctx, peer)
	stop()
	<-served
	return rows, acks, err
}

func spoolBacklog(t *testing.T, k int) (*Manager, uuid.UUID, *runnerspool.Spool) {
	t.Helper()
	m, id, spool, _ := spoolBacklogAt(t, k)
	return m, id, spool
}

// spoolBacklogAt is spoolBacklog that also returns the fixture's organisation time.
func spoolBacklogAt(t *testing.T, k int) (*Manager, uuid.UUID, *runnerspool.Spool, time.Time) {
	t.Helper()
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	for range k {
		appendDecision(t, spool, id)
	}
	return m, id, spool, org
}

func appendDecision(t *testing.T, spool *runnerspool.Spool, id uuid.UUID) {
	if err := spool.Append(context.Background(), egress.DecisionLog{Request: egress.Request{RunID: id, Host: "example.test", Time: time.Now()}, Decision: egress.Allow}); err != nil {
		t.Error(err)
	}
}

func seqs(rows []runnerspool.Record) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.Seq
	}
	return out
}

func span(from, to int64) []int64 {
	var out []int64
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

func spoolCursor(t *testing.T, spool *runnerspool.Spool) (acked int64, depth int) {
	t.Helper()
	pos, err := spool.Get(t.Context())
	d, _, _, statsErr := spool.Stats(t.Context())
	if err != nil || statsErr != nil {
		t.Fatal(err, statsErr)
	}
	return pos.Seq, d
}

func TestSpoolReplayDeliversBacklogInOrderOnce(t *testing.T) {
	const k = 2*runnerspool.ReplayBatch + 22
	m, _, spool := spoolBacklog(t, k)
	rows, _, err := replayAgainstOrg(t, m, func(int, runnerspool.Record) orgReply { return orgAck })
	if err != nil || !slices.Equal(seqs(rows), span(1, k)) {
		t.Fatalf("replay = %v, %d rows", err, len(rows))
	}
	if acked, depth := spoolCursor(t, spool); acked != k || depth != 0 {
		t.Fatalf("cursor %d depth %d after a complete replay", acked, depth)
	}
}

// A link lost mid-batch leaves the cursor at the last durably acknowledged
// batch. The next session resumes right after it: rows the org saw but never
// acknowledged are re-sent (the org deduplicates by spool seq), none is skipped.
func TestSpoolReplayResumesFromLastAckedBatch(t *testing.T) {
	const k, crash = 100, runnerspool.ReplayBatch + 7
	m, _, spool := spoolBacklog(t, k)
	first, _, err := replayAgainstOrg(t, m, func(n int, _ runnerspool.Record) orgReply {
		if n == crash {
			return orgDrop
		}
		return orgAck
	})
	if err == nil {
		t.Fatal("replay over a dropped link reported success")
	}
	if acked, _ := spoolCursor(t, spool); acked != runnerspool.ReplayBatch {
		t.Fatalf("cursor %d after a mid-batch crash", acked)
	}
	second, _, err := replayAgainstOrg(t, m, func(int, runnerspool.Record) orgReply { return orgAck })
	if err != nil || !slices.Equal(seqs(second), span(runnerspool.ReplayBatch+1, k)) {
		t.Fatalf("resume = %v, %d rows from %v", err, len(second), seqs(second[:min(1, len(second))]))
	}
	ingested := map[int64]int{}
	for _, r := range append(first, second...) {
		ingested[r.Seq]++
	}
	for n := int64(1); n <= k; n++ {
		if want := 1 + btoi(n > runnerspool.ReplayBatch && n <= crash); ingested[n] != want {
			t.Fatalf("row %d delivered %d times, want %d", n, ingested[n], want)
		}
	}
	if acked, depth := spoolCursor(t, spool); acked != k || depth != 0 {
		t.Fatalf("cursor %d depth %d after resume", acked, depth)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The org acknowledges only the last row of each batch. Replay must send the
// whole batch before waiting, so the backlog completes in one round trip per
// batch rather than one per row.
func TestSpoolReplayPipelinesEachBatch(t *testing.T) {
	const k = 2*runnerspool.ReplayBatch + 8
	m, _, spool := spoolBacklog(t, k)
	rows, acks, err := replayAgainstOrg(t, m, func(n int, _ runnerspool.Record) orgReply {
		if n%runnerspool.ReplayBatch == 0 || n == k {
			return orgAck
		}
		return orgHold
	})
	if err != nil || !slices.Equal(seqs(rows), span(1, k)) {
		t.Fatalf("replay = %v, %d rows", err, len(rows))
	}
	if acks != 3 {
		t.Fatalf("%d round trips for %d rows, want 3", acks, k)
	}
	if acked, depth := spoolCursor(t, spool); acked != k || depth != 0 {
		t.Fatalf("cursor %d depth %d", acked, depth)
	}
}

// A producer that appends a row for every row delivered never lets the spool
// tail go empty. Replay still returns once it has delivered the head it saw at
// start; later rows wait for the next replay pass.
func TestSpoolReplayBoundedByHeadAtStart(t *testing.T) {
	const k = 10
	m, id, spool := spoolBacklog(t, k)
	rows, _, err := replayAgainstOrg(t, m, func(int, runnerspool.Record) orgReply {
		appendDecision(t, spool, id)
		return orgAck
	})
	if err != nil || !slices.Equal(seqs(rows), span(1, k)) {
		t.Fatalf("live producer kept replay running: %v, %d rows", err, len(rows))
	}
	if acked, depth := spoolCursor(t, spool); acked != k || depth != k {
		t.Fatalf("cursor %d depth %d after a bounded pass", acked, depth)
	}
	rest, _, err := replayAgainstOrg(t, m, func(int, runnerspool.Record) orgReply { return orgAck })
	if err != nil || !slices.Equal(seqs(rest), span(k+1, 2*k)) {
		t.Fatalf("later appends: %v, %d rows", err, len(rest))
	}
}

// A run whose spool will not open does not stop the other runs' backlogs reaching the org; the
// replay still fails, so the runner does not come ready with a spool it never replayed.
func TestSpoolReplayGoesOnPastAnUnopenableSpool(t *testing.T) {
	m, good, _, org := spoolBacklogAt(t, 3)
	var bad uuid.UUID
	for {
		bad = createLocal(t, m, org)
		if bad.String() < good.String() {
			break
		}
	}
	spools := filepath.Join(m.dir, "spools")
	if err := os.MkdirAll(spools, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spools, bad.String()), []byte("not a spool"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, _, err := replayAgainstOrg(t, m, func(int, runnerspool.Record) orgReply { return orgAck })
	if err == nil || !strings.Contains(err.Error(), bad.String()) {
		t.Fatalf("replay = %v, want the unopenable spool named", err)
	}
	if !slices.Equal(seqs(rows), span(1, 3)) {
		t.Fatalf("the other run's backlog was %v, want 1..3", seqs(rows))
	}
}

// A quarantine that lands while a run's backlog is being replayed stops the batches after it and its exit.
func TestSpoolReplayStopsWhenTheRunIsQuarantinedMidReplay(t *testing.T) {
	const k = 3 * runnerspool.ReplayBatch
	m, id, _ := spoolBacklog(t, k)
	// An unsent exit: the held run's exit stays unsent, as for a run held before the replay began.
	m.mu.Lock()
	r := m.state.Runs[id]
	r.Exit = &runnerwire.AgentExit{}
	m.state.Runs[id] = r
	m.mu.Unlock()
	rows, _, err := replayAgainstOrg(t, m, func(n int, _ runnerspool.Record) orgReply {
		if n == 1 {
			m.mu.Lock()
			r := m.state.Runs[id]
			r.Quarantine = &types.RunnerQuarantine{}
			m.state.Runs[id] = r
			m.mu.Unlock()
		}
		return orgAck
	})
	if err != nil {
		t.Fatalf("replay = %v", err)
	}
	if len(rows) == 0 || len(rows) > runnerspool.ReplayBatch {
		t.Fatalf("%d rows reached the org, want only the batch in flight when the quarantine landed (1..%d)", len(rows), runnerspool.ReplayBatch)
	}
}
