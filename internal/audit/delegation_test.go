// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type captureRecorder struct{ got []types.AuditEvent }

func (c *captureRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	c.got = append(c.got, ev)
	return nil
}

// TestDelegationRecorderStampsEveryDelegatedRow: a row recorded under a
// delegated context names the portal whatever shape its data had, and a row
// recorded under any other context is untouched.
func TestDelegationRecorderStampsEveryDelegatedRow(t *testing.T) {
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	inner := &captureRecorder{}
	rec := DelegationRecorder{Inner: inner}
	delegated := WithDelegation(context.Background(), via)
	for _, data := range []string{``, `null`, `{"reason":"x"}`, `["not","an","object"]`, `"scalar"`} {
		if err := rec.Record(delegated, types.AuditEvent{Action: "a", Data: json.RawMessage(data)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Record(context.Background(), types.AuditEvent{Action: "b", Data: json.RawMessage(`{"reason":"y"}`)}); err != nil {
		t.Fatal(err)
	}
	for i, ev := range inner.got[:5] {
		var d struct {
			Via    *types.DelegationVia `json:"via"`
			Reason string               `json:"reason"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.Via == nil || *d.Via != via {
			t.Fatalf("row %d data = %s, want data.via naming the portal", i, ev.Data)
		}
		if i == 2 && d.Reason != "x" {
			t.Fatalf("stamping lost the row's own data: %s", ev.Data)
		}
	}
	if string(inner.got[5].Data) != `{"reason":"y"}` {
		t.Fatalf("an undelegated row was changed: %s", inner.got[5].Data)
	}
}
