// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The lost-start audit note states only what was observed: a concurrent kill
// when the write simply did not apply, a store fault when it errored (#2806).
func TestAuditLostStart_NoteMatchesTheCause(t *testing.T) {
	note := func(cause error) string {
		rec := &recRecorder{}
		s := &Server{cfg: Config{Audit: rec, Now: time.Now}}
		s.auditLostStart(context.Background(), uuid.New(), "ref", cause)
		evs := rec.snapshot()
		if len(evs) != 1 {
			t.Fatalf("rows = %d, want 1", len(evs))
		}
		return string(evs[0].Data)
	}
	if got := note(nil); !strings.Contains(got, "concurrent kill/stop") {
		t.Errorf("no cause: %s, want the concurrent kill note", got)
	}
	if got := note(errors.New("pool exhausted")); strings.Contains(got, "concurrent kill/stop") || !strings.Contains(got, "pool exhausted") {
		t.Errorf("store fault: %s, must not blame a concurrent kill and must carry the error", got)
	}
}
