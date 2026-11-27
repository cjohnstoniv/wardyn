// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The success run.kill event of an occurrence is written under the id and time the first claim pinned, with the real
// completion time in its data; a failed attempt keeps its own row (its own id, time and data), and a kill with no
// pinned receipt is left alone.
func TestKillReceiptOf(t *testing.T) {
	pinnedID, pinnedAt := uuid.New(), time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	guarded := func(rcpt killReceipt) context.Context {
		g := killGuard{}
		g.rcpt = &rcpt
		return context.WithValue(context.Background(), killGuardKey{}, g)
	}
	event := func(outcome string) types.AuditEvent {
		return types.AuditEvent{ID: uuid.New(), Time: time.Date(2026, 11, 21, 10, 5, 0, 123, time.UTC), Action: "run.kill", Outcome: outcome, Data: json.RawMessage(`{"runner":"docker"}`)}
	}

	t.Run("success row carries the pinned id and time and completed_at", func(t *testing.T) {
		ctx := guarded(killReceipt{id: pinnedID, at: pinnedAt})
		in := event("success")
		gotCtx, got := killReceiptOf(ctx, in)
		if got.ID != pinnedID || !got.Time.Equal(pinnedAt) {
			t.Errorf("success event = %s at %s, want the pinned %s at %s", got.ID, got.Time, pinnedID, pinnedAt)
		}
		var data map[string]any
		if err := json.Unmarshal(got.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["completed_at"] != in.Time.UTC().Format(time.RFC3339Nano) || data["runner"] != "docker" {
			t.Errorf("success data = %v, want completed_at %s and the runner kept", data, in.Time.UTC().Format(time.RFC3339Nano))
		}
		if gotCtx == ctx {
			t.Error("the success event is not replayed under the pinned id")
		}
	})
	t.Run("failed attempt keeps its own row", func(t *testing.T) {
		ctx := guarded(killReceipt{id: pinnedID, at: pinnedAt})
		in := event("failure")
		gotCtx, got := killReceiptOf(ctx, in)
		if got.ID != in.ID || !got.Time.Equal(in.Time) || string(got.Data) != string(in.Data) {
			t.Errorf("failure event = %s at %s data %s, want it unchanged", got.ID, got.Time, got.Data)
		}
		if gotCtx != ctx {
			t.Error("a failed attempt is replayed under the pinned id, so a retry's failure would collapse into one row")
		}
	})
	t.Run("no pinned receipt leaves the event alone", func(t *testing.T) {
		for name, ctx := range map[string]context.Context{"unguarded": context.Background(), "nothing pinned": guarded(killReceipt{})} {
			in := event("success")
			gotCtx, got := killReceiptOf(ctx, in)
			if got.ID != in.ID || string(got.Data) != string(in.Data) || gotCtx != ctx {
				t.Errorf("%s: the event or context changed", name)
			}
		}
	})
}
