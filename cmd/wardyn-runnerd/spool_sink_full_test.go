// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

// fillSpool makes the run's spool full by rows, or by bytes when byBytes: only its manifest is
// changed, the way a long offline stretch would leave it. The byte case leaves 1 MiB and 1000 rows,
// where the grant's per-record envelopes already hold back every byte of credit.
func (h *sinkHarness) fillSpool(byBytes bool) {
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
	if byBytes {
		m["last"], m["bytes"] = 1000, runnerspool.MaxBytes-runnerspool.LossReserveBytes-(1<<20)
	} else {
		m["last"] = runnerspool.MaxRecords
	}
	if b, err = json.Marshal(m); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		h.t.Fatal(err)
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
