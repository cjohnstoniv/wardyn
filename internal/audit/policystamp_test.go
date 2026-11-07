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

func stampLookup(digests map[uuid.UUID]string) func(context.Context, uuid.UUID) (types.RunPolicyState, error) {
	return func(_ context.Context, id uuid.UUID) (types.RunPolicyState, error) {
		return types.RunPolicyState{Digest: digests[id]}, nil
	}
}

func TestPolicyStampRecorder_StampsRunRows(t *testing.T) {
	run := uuid.New()
	inner := &captureRecorder{}
	rec := PolicyStampRecorder{Inner: inner, Lookup: stampLookup(map[uuid.UUID]string{run: "d1"})}
	for name, tc := range map[string]struct{ data, want string }{
		"no data":     {``, `{"policy_digest":"d1","policy_generation":1}`},
		"object data": {`{"host":"h"}`, `{"host":"h","policy_digest":"d1","policy_generation":1}`},
		"non-object":  {`[1]`, `{"data":[1],"policy_digest":"d1","policy_generation":1}`},
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
	rec := PolicyStampRecorder{Inner: inner, Lookup: func(context.Context, uuid.UUID) (types.RunPolicyState, error) {
		called = true
		return types.RunPolicyState{Digest: "d1"}, nil
	}}
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
	forged := json.RawMessage(`{"policy_digest":"forged","policy_generation":9,"k":1}`)
	for name, tc := range map[string]struct {
		run  *uuid.UUID
		want string
	}{
		"run with a digest overwritten": {&withDigest, `{"k":1,"policy_digest":"real","policy_generation":1}`},
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
	rec := PolicyStampRecorder{Inner: inner, Lookup: func(context.Context, uuid.UUID) (types.RunPolicyState, error) {
		return types.RunPolicyState{}, errors.New("db down")
	}}
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
	lookup := func(_ context.Context, id uuid.UUID) (types.RunPolicyState, error) {
		switch id {
		case withDigest:
			return types.RunPolicyState{Digest: "real"}, nil
		case without:
			return types.RunPolicyState{}, nil
		}
		return types.RunPolicyState{}, errors.New("db down")
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
		"digest overwrites it": {&withDigest, `{"k":1,"policy_digest":"real","policy_generation":1}`},
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

// revived is a run whose proxy was revived once: generation 1 (d1) on the old lineage, generation 2 (d2)
// on the fresh token the revive minted.
func revived(run uuid.UUID) func(context.Context, uuid.UUID) (types.RunPolicyState, error) {
	s, _ := types.RunPolicyState{Digest: "d1"}.Advance("d2", []string{"jti-old"}, "jti-new")
	s.LiveTokenIDs = []string{"jti-new"}
	return func(_ context.Context, id uuid.UUID) (types.RunPolicyState, error) {
		if id != run {
			return types.RunPolicyState{}, nil
		}
		return s, nil
	}
}

func stampOf(t *testing.T, ev types.AuditEvent) (string, int) {
	t.Helper()
	var d struct {
		Digest     string `json:"policy_digest"`
		Generation int    `json:"policy_generation"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.Digest, d.Generation
}

// A row the run's proxy sent is stamped with the policy of the token it presented: the fresh token's
// generation after a revive, the old lineage's for a decision made before it and delivered late, and
// unknown for a proxy row no token ties to either. A row the control plane writes on its own is under
// the current generation.
func TestPolicyStampRecorder_StampsTheTokensGeneration(t *testing.T) {
	run := uuid.New()
	for name, tc := range map[string]struct {
		ctx    context.Context
		digest string
		gen    int
	}{
		"fresh token":         {WithPolicyToken(context.Background(), run, "jti-new"), "d2", 2},
		"delayed old token":   {WithPolicyToken(context.Background(), run, "jti-old"), "d1", 1},
		"unattributable":      {WithPolicyToken(context.Background(), run, "jti-unseen"), types.PolicyDigestUnknown, 0},
		"proxy, token absent": {WithPolicyToken(context.Background(), run, ""), types.PolicyDigestUnknown, 0},
		"control plane":       {context.Background(), "d2", 2},
		"another run's token": {WithPolicyToken(context.Background(), uuid.New(), "jti-old"), "d2", 2},
	} {
		inner := &captureRecorder{}
		rec := PolicyStampRecorder{Inner: inner, Lookup: revived(run)}
		if err := rec.Record(tc.ctx, types.AuditEvent{RunID: &run, Action: "egress.allow", Data: json.RawMessage(`{"policy_generation":7}`)}); err != nil {
			t.Fatal(err)
		}
		if d, g := stampOf(t, inner.got[0]); d != tc.digest || g != tc.gen {
			t.Errorf("%s: stamped %q generation %d, want %q generation %d (data %s)", name, d, g, tc.digest, tc.gen, inner.got[0].Data)
		}
	}
}

// The generation key is the recorder's alone, like the digest: a writer's value goes even when the run
// has no policy recorded.
func TestPolicyStampRecorder_StripsWriterGeneration(t *testing.T) {
	run := uuid.New()
	inner := &captureRecorder{}
	rec := PolicyStampRecorder{Inner: inner, Lookup: stampLookup(nil)}
	for _, data := range []string{`{"policy_generation":3,"k":1}`, `{"policy\u005fgeneration":3,"k":1}`} {
		inner.got = nil
		if err := rec.Record(context.Background(), types.AuditEvent{RunID: &run, Data: json.RawMessage(data)}); err != nil {
			t.Fatal(err)
		}
		if got := string(inner.got[0].Data); got != `{"k":1}` {
			t.Errorf("%s: data = %s, want the writer's generation removed", data, got)
		}
	}
}
