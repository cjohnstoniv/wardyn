// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The states are the migration's CHECK values, and a kept machine's wire names are fixed: a run id
// and a delete start appear only when set.
func TestKeptMachineContract(t *testing.T) {
	for st, want := range map[KeptMachineState]string{
		KeptMachineCreating: "creating", KeptMachineInUse: "in_use", KeptMachineKept: "kept", KeptMachineDeleting: "deleting",
	} {
		if string(st) != want {
			t.Errorf("state %q, want %q", st, want)
		}
	}
	m := KeptMachine{ID: uuid.New(), Name: "dev", OwnerSubject: "sub-a", RunnerID: uuid.New(), Base: "base-1", State: KeptMachineKept,
		Scrubbed: true, DiskBytes: 7, ExpiresAt: time.Unix(0, 0).UTC()}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "name", "owner_subject", "runner_id", "base", "base_prepared_at", "state", "scrubbed",
		"disk_bytes", "created_at", "last_used_at", "expires_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("no %q in %s", k, raw)
		}
	}
	for _, k := range []string{"run_id", "delete_started_at"} {
		if _, ok := got[k]; ok {
			t.Errorf("%q present while unset: %s", k, raw)
		}
	}
	if got["state"] != "kept" {
		t.Errorf("state %v", got["state"])
	}
	run, now := uuid.New(), time.Now().UTC()
	m.RunID, m.DeleteStartedAt = &run, &now
	raw, _ = json.Marshal(m)
	var back KeptMachine
	if err := json.Unmarshal(raw, &back); err != nil || back.RunID == nil || *back.RunID != run || back.DeleteStartedAt == nil {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}
