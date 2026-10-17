// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type refusingRecorder struct{ err error }

func (r refusingRecorder) Record(context.Context, types.AuditEvent) error { return r.err }

// A failed store write that the spool kept is still an error, but one that
// says the event was accepted (audit.ErrSpooled) and still names the store's
// failure; with no spool to keep it, it is the store's failure alone.
func TestSpoolingRecorder_SpooledEventIsAccepted(t *testing.T) {
	down := errors.New("store unreachable")
	spool, err := api.NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ev := types.AuditEvent{ID: uuid.New(), Action: "runner.resident.erase", Outcome: "success"}

	err = spoolingRecorder{inner: refusingRecorder{down}, spool: spool}.Record(t.Context(), ev)
	if !errors.Is(err, audit.ErrSpooled) || !errors.Is(err, down) {
		t.Fatalf("spooled write = %v, want ErrSpooled wrapping the store's error", err)
	}
	if spool.Lines() != 1 {
		t.Fatalf("spool holds %d lines, want the event", spool.Lines())
	}
	err = spoolingRecorder{inner: refusingRecorder{down}}.Record(t.Context(), ev)
	if errors.Is(err, audit.ErrSpooled) || !errors.Is(err, down) {
		t.Fatalf("unspooled write = %v, want the store's error and no ErrSpooled", err)
	}
	if err := (spoolingRecorder{inner: refusingRecorder{}, spool: spool}).Record(t.Context(), ev); err != nil || spool.Lines() != 1 {
		t.Fatalf("stored write = %v with %d spool lines, want nil and nothing spooled", err, spool.Lines())
	}
}
