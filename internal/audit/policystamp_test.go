// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func stampLookup(digests map[uuid.UUID]string) func(context.Context, uuid.UUID) (string, error) {
	return func(_ context.Context, id uuid.UUID) (string, error) { return digests[id], nil }
}

func TestPolicyStampRecorder_StampsRunRows(t *testing.T) {
	run := uuid.New()
	inner := &captureRecorder{}
	rec := PolicyStampRecorder{Inner: inner, Lookup: stampLookup(map[uuid.UUID]string{run: "d1"})}
	for name, tc := range map[string]struct{ data, want string }{
		"no data":     {``, `{"policy_digest":"d1"}`},
		"object data": {`{"host":"h"}`, `{"host":"h","policy_digest":"d1"}`},
		"non-object":  {`[1]`, `{"data":[1],"policy_digest":"d1"}`},
	} {
		inner.got = nil
		if err := rec.Record(context.Background(), types.AuditEvent{RunID: &run, Action: "egress.allow", Data: json.RawMessage(tc.data)}); err != nil {
			t.Fatal(err)
		}
		if got := string(inner.got[0].Data); got != tc.want {
			t.Errorf("%s: data = %s, want %s", name, got, tc.want)
		}
	}
}

func TestPolicyStampRecorder_SkipsRunless(t *testing.T) {
	inner := &captureRecorder{}
	called := false
	rec := PolicyStampRecorder{Inner: inner, Lookup: func(context.Context, uuid.UUID) (string, error) { called = true; return "d1", nil }}
	if err := rec.Record(context.Background(), types.AuditEvent{Action: "policy.inline.apply", Data: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("a row with no run read a digest")
	}
	if got := string(inner.got[0].Data); got != `{"a":1}` {
		t.Errorf("data = %s, want it untouched", got)
	}
}

// Only the recorder sets the key: a writer's own value is removed whether the
// run has a digest, has none, or has no row at all.
func TestPolicyStampRecorder_StripsWriterKey(t *testing.T) {
	withDigest, without, runless := uuid.New(), uuid.New(), (*uuid.UUID)(nil)
	inner := &captureRecorder{}
	rec := PolicyStampRecorder{Inner: inner, Lookup: stampLookup(map[uuid.UUID]string{withDigest: "real"})}
	forged := json.RawMessage(`{"policy_digest":"forged","k":1}`)
	for name, tc := range map[string]struct {
		run  *uuid.UUID
		want string
	}{
		"run with a digest overwritten": {&withDigest, `{"k":1,"policy_digest":"real"}`},
		"run before its digest":         {&without, `{"k":1}`},
		"no run":                        {runless, `{"k":1}`},
	} {
		inner.got = nil
		if err := rec.Record(context.Background(), types.AuditEvent{RunID: tc.run, Data: forged}); err != nil {
			t.Fatal(err)
		}
		if got := string(inner.got[0].Data); got != tc.want {
			t.Errorf("%s: data = %s, want %s", name, got, tc.want)
		}
	}
}

// A failed lookup never drops or delays a row; it is written without the key.
func TestPolicyStampRecorder_LookupErrorKeepsRow(t *testing.T) {
	run := uuid.New()
	inner := &captureRecorder{}
	rec := PolicyStampRecorder{Inner: inner, Lookup: func(context.Context, uuid.UUID) (string, error) { return "", errors.New("db down") }}
	if err := rec.Record(context.Background(), types.AuditEvent{RunID: &run, Action: "egress.deny", Data: json.RawMessage(`{"policy_digest":"forged","k":1}`)}); err != nil {
		t.Fatalf("Record = %v, want the row kept", err)
	}
	if len(inner.got) != 1 || string(inner.got[0].Data) != `{"k":1}` {
		t.Errorf("got %+v, want one row without the key", inner.got)
	}
}

// The coalescer sits above the stamp, so its summary rows are stamped too.
func TestPolicyStampRecorder_CoalescedSummaryStamped(t *testing.T) {
	run := uuid.New()
	inner := &captureRecorder{}
	c := &DenialCoalescer{Inner: PolicyStampRecorder{Inner: inner, Lookup: stampLookup(map[uuid.UUID]string{run: "d1"})}}
	ctx := WithDryRun(context.Background())
	for range 3 {
		data, _ := json.Marshal(map[string]string{"reason": "r"})
		if err := c.Record(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &run, ActorType: types.ActorHuman, Actor: "a",
			Action: DenialAction, Target: "t", Outcome: DenialOutcome, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	c.Flush(context.Background())
	if len(inner.got) != 2 || inner.got[1].Action != CoalesceAction {
		t.Fatalf("recorded %d rows, want the first denial and a summary", len(inner.got))
	}
	for _, ev := range inner.got {
		var d struct {
			PolicyDigest string `json:"policy_digest"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.PolicyDigest != "d1" {
			t.Errorf("%s row data = %s, want policy_digest d1", ev.Action, ev.Data)
		}
	}
}

// The reserved key is read as JSON reads it, so an escaped spelling of it is removed too.
func TestPolicyStampRecorder_StripsEscapedWriterKey(t *testing.T) {
	withDigest, without := uuid.New(), uuid.New()
	lookup := func(_ context.Context, id uuid.UUID) (string, error) {
		switch id {
		case withDigest:
			return "real", nil
		case without:
			return "", nil
		}
		return "", errors.New("db down")
	}
	failed := uuid.New()
	forged := json.RawMessage(`{"policy\u005fdigest":"forged","k":1}`)
	for name, tc := range map[string]struct {
		run  *uuid.UUID
		want string
	}{
		"no run":               {nil, `{"k":1}`},
		"empty digest":         {&without, `{"k":1}`},
		"failed lookup":        {&failed, `{"k":1}`},
		"digest overwrites it": {&withDigest, `{"k":1,"policy_digest":"real"}`},
	} {
		inner := &captureRecorder{}
		if err := (PolicyStampRecorder{Inner: inner, Lookup: lookup}).Record(context.Background(), types.AuditEvent{RunID: tc.run, Data: forged}); err != nil {
			t.Fatal(err)
		}
		if got := string(inner.got[0].Data); got != tc.want {
			t.Errorf("%s: data = %s, want %s", name, got, tc.want)
		}
	}
}
