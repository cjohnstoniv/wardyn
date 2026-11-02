// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"testing"
)

// The pause generation crosses the wire both ways between versions. A runner that predates it reads a
// new wardynd's pause_agent as the RefArgs it knew, ignoring the generation; a new runner reads an old
// wardynd's (no generation) as generation 0; and a new wardynd reads an old runner's empty answer, and
// a new runner's for an old guest agent, as fenced_pause false.
func TestAgentPauseArgsAndResultCrossVersions(t *testing.T) {
	b, err := json.Marshal(AgentPauseArgs{Ref: "r-1", Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	var old RefArgs
	if err := Unmarshal(b, &old); err != nil || old.Ref != "r-1" {
		t.Fatalf("an old runner reads %s as %+v, %v", b, old, err)
	}
	b, err = json.Marshal(RefArgs{Ref: "r-1"})
	if err != nil {
		t.Fatal(err)
	}
	var args AgentPauseArgs
	if err := Unmarshal(b, &args); err != nil || args != (AgentPauseArgs{Ref: "r-1"}) {
		t.Fatalf("a new runner reads an old wardynd's %s as %+v, %v", b, args, err)
	}
	if b, _ := json.Marshal(AgentPauseArgs{Ref: "r-1"}); string(b) != `{"ref":"r-1"}` {
		t.Fatalf("a pause with no generation = %s, want the old shape", b)
	}
	for _, answer := range []string{`null`, `{}`} {
		var res AgentPauseResult
		if err := Unmarshal([]byte(answer), &res); err != nil || res.FencedPause {
			t.Fatalf("answer %s = %+v, %v; want fenced_pause false", answer, res, err)
		}
	}
	var res AgentPauseResult
	if err := Unmarshal([]byte(`{"fenced_pause":true,"later":1}`), &res); err != nil || !res.FencedPause {
		t.Fatalf("a fenced answer = %+v, %v", res, err)
	}
}
