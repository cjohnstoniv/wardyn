// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRecordFlag_TriStateAcrossPeers is the per-run recording contract between peers of different
// ages. A new organisation's choice reaches a new runner as sent; an old organisation sends no field,
// which a new runner reads as its own default; an old runner decodes a new organisation's create.
func TestRecordFlag_TriStateAcrossPeers(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name     string
		sent     *bool
		ceiling  bool
		recorded bool
	}{
		{"new org asks, runner records", &on, true, true},
		{"new org declines, runner does not record", &off, true, false},
		{"old org (no field), runner records by its own default", nil, true, true},
		{"new org asks, runner ceiling off", &on, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			create(t, rig, func(s *runner.SandboxSpec) { s.Record = tc.sent })
			got := rig.Fake.Created[len(rig.Fake.Created)-1]
			if (got.Record == nil) != (tc.sent == nil) || (got.Record != nil && *got.Record != *tc.sent) {
				t.Fatalf("record on the runner = %v, sent %v", got.Record, tc.sent)
			}
			if r := got.RecordWithin(tc.ceiling); r != tc.recorded {
				t.Fatalf("recorded = %v, want %v", r, tc.recorded)
			}
		})
	}

	// The field is absent from an old organisation's create, and an old runner (a spec with no
	// record field) decodes a new organisation's create unchanged.
	old, err := json.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: uuid.New()}})
	if err != nil || bytes.Contains(old, []byte(`"record"`)) {
		t.Fatalf("a create with no choice carries a record field: %s, %v", old, err)
	}
	sent, _ := json.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: uuid.New(), Record: &off}})
	if !bytes.Contains(sent, []byte(`"record":false`)) {
		t.Fatalf("a declined run's create = %s, want record:false", sent)
	}
	var oldRunner struct {
		Spec struct{ RunID uuid.UUID } `json:"spec"`
	}
	if err := runnerwire.Unmarshal(sent, &oldRunner); err != nil || oldRunner.Spec.RunID == uuid.Nil {
		t.Fatalf("an old runner refuses a new organisation's create: %v", err)
	}
}

// TestRecordFlag_OldRunnerDoesNotRecordOnTheOrgsWord: a runner that does not advertise the per-run
// capability records by its own setting whatever the organisation asks, so the organisation's facts
// say it does not record on its word; one that advertises it and records does.
func TestRecordFlag_OldRunnerDoesNotRecordOnTheOrgsWord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		support    substrate.ClassSupport
		wantRecord bool
	}{
		{"old runner", substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}, SessionRecording: true}, false},
		{"new runner, recording on", substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}, SessionRecording: true, RecordPerRun: true}, true},
		{"new runner, --recording-store off", substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}, RecordPerRun: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			data, err := json.Marshal(runnerwire.Caps{Support: tc.support})
			if err != nil {
				t.Fatal(err)
			}
			rig.Sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
			f, ok := rig.Sub.Advertised()
			if !ok || f.Records != tc.wantRecord {
				t.Fatalf("Advertised = %+v, %v; want Records %v", f, ok, tc.wantRecord)
			}
		})
	}
}
