// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWorkloadReportValidate(t *testing.T) {
	spec := json.RawMessage(`{"intent":{}}`)
	ok := []WorkloadReport{
		{Op: WorkloadOpApply, Namespace: "team-a", Name: "triage", UID: "u1", Generation: 1, Spec: spec},
		{Op: WorkloadOpDelete, Namespace: "team-a", Name: "triage", UID: "u1"},
		{Op: WorkloadOpSync, Namespace: "team-a", Names: []string{"triage", "bot"}},
		{Op: WorkloadOpSync, Namespace: "team-a"},
	}
	for i, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]WorkloadReport{
		"unknown op":          {Op: "patch", Namespace: "team-a"},
		"bad namespace":       {Op: WorkloadOpSync, Namespace: "Team_A"},
		"no namespace":        {Op: WorkloadOpSync},
		"apply no spec":       {Op: WorkloadOpApply, Namespace: "team-a", Name: "t", UID: "u", Generation: 1},
		"apply no generation": {Op: WorkloadOpApply, Namespace: "team-a", Name: "t", UID: "u", Spec: spec},
		"apply no uid":        {Op: WorkloadOpApply, Namespace: "team-a", Name: "t", Generation: 1, Spec: spec},
		"apply bad name":      {Op: WorkloadOpApply, Namespace: "team-a", Name: "-t", UID: "u", Generation: 1, Spec: spec},
		"apply with names":    {Op: WorkloadOpApply, Namespace: "team-a", Name: "t", UID: "u", Generation: 1, Spec: spec, Names: []string{"x"}},
		"apply spec too big":  {Op: WorkloadOpApply, Namespace: "team-a", Name: "t", UID: "u", Generation: 1, Spec: json.RawMessage(`"` + strings.Repeat("a", MaxWorkloadSpecBytes) + `"`)},
		"delete with spec":    {Op: WorkloadOpDelete, Namespace: "team-a", Name: "t", UID: "u", Spec: spec},
		"sync with name":      {Op: WorkloadOpSync, Namespace: "team-a", Name: "t"},
		"sync bad name":       {Op: WorkloadOpSync, Namespace: "team-a", Names: []string{"ok", "No"}},
		"sync too many":       {Op: WorkloadOpSync, Namespace: "team-a", Names: make([]string, MaxWorkloadSyncNames+1)},
	}
	for name, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: want ErrBadFrame, got %v", name, err)
		}
	}
}

// A workload name is pkg/client's grammar: a DNS label that starts with a letter. A namespace is any
// DNS label, so it may start with a digit.
func TestWorkloadNameBoundaries(t *testing.T) {
	spec := json.RawMessage(`{"intent":{}}`)
	apply := func(ns, name string) error {
		return WorkloadReport{Op: WorkloadOpApply, Namespace: ns, Name: name, UID: "u", Generation: 1, Spec: spec}.Validate()
	}
	for _, name := range []string{"w", "w-1", "a-b-c", "a" + strings.Repeat("b", 62)} {
		if err := apply("team-a", name); err != nil {
			t.Errorf("name %q: %v", name, err)
		}
		if err := (WorkloadReport{Op: WorkloadOpSync, Namespace: "team-a", Names: []string{name}}).Validate(); err != nil {
			t.Errorf("sync name %q: %v", name, err)
		}
		if err := (WorkloadStatusArgs{Namespace: "team-a", Name: name, Phase: WorkloadPhases[0]}).Validate(); err != nil {
			t.Errorf("status name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "-w", "w-", "1w", "W", "a" + strings.Repeat("b", 63)} {
		if err := apply("team-a", name); !errors.Is(err, ErrBadFrame) {
			t.Errorf("name %q: want ErrBadFrame, got %v", name, err)
		}
		if err := (WorkloadReport{Op: WorkloadOpSync, Namespace: "team-a", Names: []string{name}}).Validate(); !errors.Is(err, ErrBadFrame) {
			t.Errorf("sync name %q: want ErrBadFrame, got %v", name, err)
		}
		if err := (WorkloadStatusArgs{Namespace: "team-a", Name: name, Phase: WorkloadPhases[0]}).Validate(); !errors.Is(err, ErrBadFrame) {
			t.Errorf("status name %q: want ErrBadFrame, got %v", name, err)
		}
	}
	if err := apply("1-team", "w"); err != nil {
		t.Errorf("a digit-led namespace: %v", err)
	}
}

func TestWorkloadStatusArgsValidate(t *testing.T) {
	for _, p := range WorkloadPhases {
		if err := (WorkloadStatusArgs{Namespace: "team-a", Name: "triage", ObservedGeneration: 4, Phase: p}).Validate(); err != nil {
			t.Errorf("phase %s: %v", p, err)
		}
	}
	if len(WorkloadPhases) != 10 {
		t.Fatalf("%d phases, want the 10 the CRD status names", len(WorkloadPhases))
	}
	bad := map[string]WorkloadStatusArgs{
		"unknown phase":   {Namespace: "team-a", Name: "t", Phase: "Done"},
		"no name":         {Namespace: "team-a", Phase: WorkloadPhaseRunning},
		"negative gen":    {Namespace: "team-a", Name: "t", Phase: WorkloadPhaseRunning, ObservedGeneration: -1},
		"message too big": {Namespace: "team-a", Name: "t", Phase: WorkloadPhaseRefused, Message: strings.Repeat("m", MaxWorkloadMessage+1)},
	}
	for name, a := range bad {
		if err := a.Validate(); !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: want ErrBadFrame, got %v", name, err)
		}
	}
}

// A largest apply report, wrapped as the EVENT it travels in, fits one control frame.
func TestWorkloadEventFitsControlFrame(t *testing.T) {
	spec, err := json.Marshal(map[string]string{"intent": strings.Repeat("a", MaxWorkloadSpecBytes-len(`{"intent":""}`))})
	if err != nil {
		t.Fatal(err)
	}
	rep := WorkloadReport{Op: WorkloadOpApply, Namespace: strings.Repeat("n", 63), Name: strings.Repeat("w", 63), UID: uuid.NewString(), Generation: 1 << 40, Spec: spec}
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := Marshal(Event{Kind: EventWorkload, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > MaxControlFrame {
		t.Fatalf("a %d-byte spec made a %d-byte EVENT, over the %d control frame", len(spec), len(payload), MaxControlFrame)
	}
	f := Frame{Type: TypeEvent, Seq: 1, Stream: 0, Payload: payload}
	if err := f.Validate(); err != nil {
		t.Fatalf("frame: %v", err)
	}
	// A sync at its name cap fits too.
	names := make([]string, MaxWorkloadSyncNames)
	for i := range names {
		names[i] = strings.Repeat("s", 63)
	}
	sync, _ := Marshal(WorkloadReport{Op: WorkloadOpSync, Namespace: "team-a", Names: names})
	if len(sync) > MaxControlFrame {
		t.Fatalf("a full sync is %d bytes", len(sync))
	}
}

func TestWorkloadEventKindAndCapsRoundTrip(t *testing.T) {
	if err := (Event{Kind: EventWorkload}).Validate(); err != nil {
		t.Fatal(err)
	}
	var back Caps
	raw, err := json.Marshal(Caps{RunsNamespaces: []string{"wardyn-runs", "team-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &back); err != nil || len(back.RunsNamespaces) != 2 || back.RunsNamespaces[0] != "wardyn-runs" {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
	// An older runner says nothing: the field is absent from the wire.
	old, _ := json.Marshal(Caps{})
	if bytes.Contains(old, []byte("runs_namespaces")) {
		t.Fatalf("empty caps carry runs_namespaces: %s", old)
	}
}
