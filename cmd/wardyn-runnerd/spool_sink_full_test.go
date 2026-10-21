// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

// fillSpool makes the run's spool full by rows, or by bytes when byBytes: only its manifest is
// changed, the way a long offline stretch would leave it. By rows, no room is left for a record. By
// bytes, a session leaves the credit 100 bytes above the smallest request's envelope and the proxy
// is refused against it (the stall real requests hit; see tightenBytes and refuse).
func (h *sinkHarness) fillSpool(byBytes bool) {
	h.t.Helper()
	if byBytes {
		c, gen := h.tightenBytes()
		h.refuse(c, gen)
		return
	}
	h.editManifest(func(m map[string]any) { m["last"] = runnerspool.MaxRecords })
}

func (h *sinkHarness) editManifest(edit func(map[string]any)) {
	h.t.Helper()
	p := filepath.Join(h.dir, "spools", h.id.String(), "state.json")
	b, err := os.ReadFile(p)
	if err != nil {
		h.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		h.t.Fatal(err)
	}
	edit(m)
	if b, err = json.Marshal(m); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// tightenBytes opens a proxy session, leaves the spool 100 bytes of credit above the smallest request
// envelope and has the sink regrant: the proxy now holds a grant no real request fits.
func (h *sinkHarness) tightenBytes() (*tls.Conn, uint64) {
	h.t.Helper()
	c := h.dial(h.key, inst)
	acc := mustRead(h.t, c, proxy.FrameAccept)
	mustRead(h.t, c, proxy.FrameGrant)
	h.editManifest(func(m map[string]any) {
		m["last"], m["bytes"] = 10, runnerspool.MaxBytes-runnerspool.LossReserveBytes-runnerspool.GrantRecords*512-(runnerspool.MinRequestReservation+100)
	})
	h.sink.Tick(h.t.Context())
	mustRead(h.t, c, proxy.FrameGrant)
	return c, acc.Gen
}

// refuse sends the proxy's refused frame and waits until the spool counts it.
func (h *sinkHarness) refuse(c *tls.Conn, gen uint64) {
	h.t.Helper()
	if err := proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameRefused, Gen: gen, Total: 1}); err != nil {
		h.t.Fatal(err)
	}
	sp := h.spool()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if full, err := sp.Full(h.t.Context()); err == nil && full {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("the spool never counted the proxy's refusal")
		}
	}
}

type endCalls struct {
	mu    sync.Mutex
	runs  []uuid.UUID
	isVM  bool
	fails int
	hold  chan struct{} // when set, end waits for it to close
}

func (e *endCalls) end(_ context.Context, id uuid.UUID) (bool, error) {
	if e.hold != nil {
		<-e.hold
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runs = append(e.runs, id)
	if e.fails > 0 {
		e.fails--
		return false, errors.New("the engine did not answer")
	}
	return e.isVM, nil
}

// A full spool ends the run's VM, once; a spool with room, or no VM switch, ends nothing.
func TestSpoolSinkEndsAVMWhoseSpoolIsFull(t *testing.T) {
	ctx := t.Context()
	t.Run("room left", func(t *testing.T) {
		h := newSinkHarness(t)
		e := &endCalls{isVM: true}
		h.sink.EndVMsOnFullSpool(e.end)
		h.sink.EndVMsWithFullSpools(ctx)
		if len(e.runs) != 0 {
			t.Fatalf("a spool with room ended %v", e.runs)
		}
	})
	for _, byBytes := range []bool{false, true} {
		name := "full by rows, once"
		if byBytes {
			name = "full by bytes, once"
		}
		t.Run(name, func(t *testing.T) {
			h := newSinkHarness(t)
			e := &endCalls{isVM: true}
			h.sink.EndVMsOnFullSpool(e.end)
			h.fillSpool(byBytes)
			h.sink.EndVMsWithFullSpools(ctx)
			h.sink.EndVMsWithFullSpools(ctx)
			if len(e.runs) != 1 || e.runs[0] != h.id {
				t.Fatalf("a full spool ended %v, want the run once", e.runs)
			}
		})
	}
	t.Run("a failed end leaves the run unhandled and is tried again", func(t *testing.T) {
		h := newSinkHarness(t)
		e := &endCalls{isVM: true, fails: 1}
		h.sink.EndVMsOnFullSpool(e.end)
		h.fillSpool(false)
		handled := func() bool {
			h.sink.mu.Lock()
			defer h.sink.mu.Unlock()
			return h.sink.runs[h.id].fullHandled
		}
		h.sink.EndVMsWithFullSpools(ctx)
		if handled() {
			t.Fatal("a run whose VM could not be ended (or identified) was marked handled")
		}
		h.sink.EndVMsWithFullSpools(ctx)
		if !handled() {
			t.Fatal("the run was not marked handled once its VM ended")
		}
		h.sink.EndVMsWithFullSpools(ctx)
		if len(e.runs) != 2 {
			t.Fatalf("end called %d times, want the failure retried once and then left", len(e.runs))
		}
	})
	t.Run("a container run is asked once and left", func(t *testing.T) {
		h := newSinkHarness(t)
		e := &endCalls{}
		h.sink.EndVMsOnFullSpool(e.end)
		h.fillSpool(false)
		h.sink.EndVMsWithFullSpools(ctx)
		h.sink.EndVMsWithFullSpools(ctx)
		if len(e.runs) != 1 {
			t.Fatalf("end called %d times for a container run, want 1", len(e.runs))
		}
	})
	t.Run("no VM switch", func(t *testing.T) {
		h := newSinkHarness(t)
		h.fillSpool(false)
		h.sink.EndVMsWithFullSpools(ctx) // nothing set: must not panic or end anything
	})
}

// A VM end that keeps failing (the engine cannot inspect it, or its label is malformed) is retried on
// every tick but logged once, then once per interval, not once a second for the life of the run.
func TestSpoolSinkLogsAFailingEndRarely(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	prevEvery := endWarnEvery
	endWarnEvery = time.Hour
	t.Cleanup(func() { slog.SetDefault(prev); endWarnEvery = prevEvery })
	warns := func() int { return strings.Count(logs.String(), "will retry") }

	h := newSinkHarness(t)
	e := &endCalls{isVM: true, fails: 100}
	h.sink.EndVMsOnFullSpool(e.end)
	h.fillSpool(false)
	for range 5 {
		h.sink.EndVMsWithFullSpools(t.Context())
	}
	e.mu.Lock()
	calls := len(e.runs)
	e.mu.Unlock()
	if calls != 5 || warns() != 1 {
		t.Fatalf("5 failing ticks made %d end calls and %d warnings, want 5 and 1:\n%s", calls, warns(), logs.String())
	}
	if !strings.Contains(logs.String(), h.id.String()) {
		t.Fatalf("the warning does not name the run:\n%s", logs.String())
	}
	endWarnEvery = 20 * time.Millisecond
	time.Sleep(40 * time.Millisecond)
	h.sink.EndVMsWithFullSpools(t.Context())
	if warns() != 2 {
		t.Fatalf("after the interval the failure was logged %d times, want 2", warns())
	}
}

// A VM whose spool stalls on a real request (credit left above the smallest envelope, the proxy
// refused against it) is ended by the sink: the proxy's refused frame, not a constant, says the spool
// is full. The proxy side of the same state is pinned in internal/egress/proxy against real
// reservations.
func TestSpoolSinkEndsAVMOnceTheProxyIsRefused(t *testing.T) {
	ctx := t.Context()
	h := newSinkHarness(t)
	e := &endCalls{isVM: true}
	h.sink.EndVMsOnFullSpool(e.end)
	c, gen := h.tightenBytes()
	h.sink.EndVMsWithFullSpools(ctx)
	if len(e.runs) != 0 {
		t.Fatalf("a tight spool nobody was refused by ended %v", e.runs)
	}
	h.refuse(c, gen)
	h.sink.EndVMsWithFullSpools(ctx)
	if len(e.runs) != 1 || e.runs[0] != h.id {
		t.Fatalf("a spool the proxy was refused by ended %v, want the run once", e.runs)
	}
}

// A pass a restart cut short is finished by the next start, whatever the spool says by then: the VM's
// proxy reconnects, its Accept clears what made the spool full, and only the mark the pass left in the
// spool says the VM is still to be ended.
func TestSpoolSinkFinishesAnEndARestartCutShort(t *testing.T) {
	ctx := t.Context()
	h := newSinkHarness(t)
	cut := &endCalls{isVM: true, fails: 1} // the pass is cancelled mid-end
	h.sink.EndVMsOnFullSpool(cut.end)
	h.fillSpool(false)
	h.sink.EndVMsWithFullSpools(ctx)
	if len(cut.runs) != 1 {
		t.Fatalf("the cut-short pass made %d end calls, want 1", len(cut.runs))
	}
	// The restart: the run is registered again with a fresh in-memory state, and by then the spool
	// has room (a reconnecting proxy's Accept regrants).
	h.editManifest(func(m map[string]any) { m["last"] = 0 })
	hash := h.sink.runs[h.id].keyHash
	h.sink.Unregister(h.id)
	if err := h.sink.Register(h.id, hash); err != nil {
		t.Fatal(err)
	}
	if full, err := h.spool().Full(ctx); err != nil || full {
		t.Fatalf("Full after the restart = %v, %v; the scenario needs a spool with room", full, err)
	}
	next := &endCalls{isVM: true}
	h.sink.EndVMsOnFullSpool(next.end)
	h.sink.EndVMsWithFullSpools(ctx)
	if len(next.runs) != 1 || next.runs[0] != h.id {
		t.Fatalf("after the restart the pass ended %v, want the run once", next.runs)
	}
	if pending, err := h.spool().EndPending(ctx); err != nil || pending {
		t.Fatalf("the mark after the VM was ended = %v, %v; want cleared", pending, err)
	}
	h.sink.EndVMsWithFullSpools(ctx)
	if len(next.runs) != 1 {
		t.Fatalf("a finished end was run again: %v", next.runs)
	}
}
