// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A run launched through a portal that named an agent shows the agent as created_via_actor beside
// created_via. A run with no agent writes no such key, so every other run reads as it did.
func TestRunJSONCreatedViaActor(t *testing.T) {
	portal := uuid.New()
	run := AgentRun{ID: uuid.New(), CreatedVia: &portal, CreatedViaActor: &DelegationActor{Sub: "agent-sub", Iss: "https://idp.example"}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if got := string(m["created_via_actor"]); got != `{"sub":"agent-sub","iss":"https://idp.example"}` {
		t.Errorf("created_via_actor = %s", got)
	}
	var back AgentRun
	if err := json.Unmarshal(raw, &back); err != nil || back.CreatedViaActor == nil || *back.CreatedViaActor != *run.CreatedViaActor {
		t.Errorf("round trip = %+v err=%v", back.CreatedViaActor, err)
	}

	run.CreatedViaActor = nil
	raw, _ = json.Marshal(run)
	if strings.Contains(string(raw), "created_via_actor") {
		t.Errorf("a run with no agent wrote created_via_actor: %s", raw)
	}
}
