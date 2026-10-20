// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

// fillSpool makes the run's spool hold as many records as it can: only its manifest is changed, the
// way a long offline stretch would leave it.
func (h *sinkHarness) fillSpool() {
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
	m["last"] = runnerspool.MaxRecords
	if b, err = json.Marshal(m); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

type endCalls struct {
	runs  []uuid.UUID
	isVM  bool
	fails int
}

func (e *endCalls) end(_ context.Context, id uuid.UUID) (bool, error) {
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
	t.Run("full, once", func(t *testing.T) {
		h := newSinkHarness(t)
		e := &endCalls{isVM: true}
		h.sink.EndVMsOnFullSpool(e.end)
		h.fillSpool()
		h.sink.EndVMsWithFullSpools(ctx)
		h.sink.EndVMsWithFullSpools(ctx)
		if len(e.runs) != 1 || e.runs[0] != h.id {
			t.Fatalf("a full spool ended %v, want the run once", e.runs)
		}
	})
	t.Run("a failed end is tried again", func(t *testing.T) {
		h := newSinkHarness(t)
		e := &endCalls{isVM: true, fails: 1}
		h.sink.EndVMsOnFullSpool(e.end)
		h.fillSpool()
		h.sink.EndVMsWithFullSpools(ctx)
		h.sink.EndVMsWithFullSpools(ctx)
		h.sink.EndVMsWithFullSpools(ctx)
		if len(e.runs) != 2 {
			t.Fatalf("end called %d times, want the failure retried once and then left", len(e.runs))
		}
	})
	t.Run("a container run is asked once and left", func(t *testing.T) {
		h := newSinkHarness(t)
		e := &endCalls{}
		h.sink.EndVMsOnFullSpool(e.end)
		h.fillSpool()
		h.sink.EndVMsWithFullSpools(ctx)
		h.sink.EndVMsWithFullSpools(ctx)
		if len(e.runs) != 1 {
			t.Fatalf("end called %d times for a container run, want 1", len(e.runs))
		}
	})
	t.Run("no VM switch", func(t *testing.T) {
		h := newSinkHarness(t)
		h.fillSpool()
		h.sink.EndVMsWithFullSpools(ctx) // nothing set: must not panic or end anything
	})
}
