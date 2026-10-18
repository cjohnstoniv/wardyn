// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
)

// A runner held at the asserted-subject cap reconnects about once a minute: its hold is audited when it
// begins and again once an hour, not on every reconnect, and one runner's rows never space another's.
func TestRecordRunnerInventoryHoldIsSpacedPerRunner(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	h.srv.cfg.Now = func() time.Time { return now }
	held := func(id uuid.UUID) int {
		n := 0
		for _, ev := range h.audit.snapshot() {
			if ev.Action == placement.ActionRunnerInventoryHold && ev.Target == id.String() {
				n++
			}
		}
		return n
	}
	a, b := uuid.New(), uuid.New()
	for range 90 { // one reconnect a minute for an hour and a half
		h.srv.RecordRunnerInventoryHold(context.Background(), a)
		now = now.Add(time.Minute)
	}
	if got := held(a); got != 2 {
		t.Fatalf("%d rows for 90 reconnects over 89 minutes, want the first and one more an hour later", got)
	}
	h.srv.RecordRunnerInventoryHold(context.Background(), b)
	if got := held(b); got != 1 {
		t.Fatalf("a second runner's first hold wrote %d rows, want 1", got)
	}
}
