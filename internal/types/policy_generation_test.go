// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

// dispatched is a run's state after dispatch: one policy, its proxy on the token it started with, renewed once.
func dispatched() RunPolicyState {
	return RunPolicyState{Digest: "d1", LiveTokenIDs: []string{"jti-1b", "jti-1a"}}
}

func TestRunPolicyState_OneGenerationStampsEveryRow(t *testing.T) {
	s := dispatched()
	for name, tc := range map[string]struct {
		fromProxy bool
		token     string
	}{
		"control plane":       {false, ""},
		"proxy, live token":   {true, "jti-1b"},
		"proxy, other token":  {true, "jti-unseen"},
		"proxy, token absent": {true, ""},
	} {
		if got := s.Stamp(tc.fromProxy, tc.token); got != (PolicyStamp{Digest: "d1", Generation: 1}) {
			t.Errorf("%s: stamp = %+v, want d1 generation 1", name, got)
		}
	}
	if got := (RunPolicyState{}).Stamp(true, "x"); got != (PolicyStamp{}) {
		t.Errorf("a run with no recorded policy stamps %+v, want nothing", got)
	}
}

// Tighten, revive, act: the revive starts generation 2 under the new digest, and a decision the new
// proxy makes on its fresh token, a renewal of it, or a row the control plane writes after the revive
// all carry generation 2.
func TestRunPolicyState_TightenReviveAction(t *testing.T) {
	s, gen := dispatched().Advance("d2", []string{"jti-1b", "jti-1a", "jti-0"}, "jti-2")
	if gen != 2 || s.Digest != "d2" {
		t.Fatalf("advance = generation %d digest %q, want 2 and d2", gen, s.Digest)
	}
	s.LiveTokenIDs = []string{"jti-2"} // the revive cut the lineage to the fresh token
	want := PolicyStamp{Digest: "d2", Generation: 2}
	if got := s.Stamp(true, "jti-2"); got != want {
		t.Errorf("fresh token: stamp %+v, want %+v", got, want)
	}
	if got := s.Stamp(false, ""); got != want {
		t.Errorf("control plane after the revive: stamp %+v, want %+v", got, want)
	}
	s.LiveTokenIDs = []string{"jti-2b", "jti-2"} // the new proxy renewed
	if got := s.Stamp(true, "jti-2b"); got != want {
		t.Errorf("renewed token: stamp %+v, want %+v", got, want)
	}
}

// A decision the old proxy made before the revive and delivered after it presents a token of the old
// lineage: it keeps generation 1, whichever of that lineage's tokens it carried.
func TestRunPolicyState_DelayedPreReviveDecisionKeepsItsGeneration(t *testing.T) {
	s, _ := dispatched().Advance("d2", []string{"jti-1b", "jti-1a", "jti-0"}, "jti-2")
	s.LiveTokenIDs = []string{"jti-2"}
	for _, tok := range []string{"jti-0", "jti-1a", "jti-1b"} {
		if got := s.Stamp(true, tok); got != (PolicyStamp{Digest: "d1", Generation: 1}) {
			t.Errorf("old token %s: stamp %+v, want d1 generation 1", tok, got)
		}
	}
}

// Once a run has had two policies, a proxy row no token ties to either is unknown, never the original.
func TestRunPolicyState_UnattributableIsUnknown(t *testing.T) {
	s, _ := dispatched().Advance("d2", []string{"jti-1b"}, "jti-2")
	s.LiveTokenIDs = []string{"jti-2"}
	for name, tok := range map[string]string{"unseen token": "jti-x", "no token": ""} {
		if got := s.Stamp(true, tok); got != (PolicyStamp{Digest: PolicyDigestUnknown}) {
			t.Errorf("%s: stamp %+v, want unknown with no generation", name, got)
		}
	}
}

// A revive that never replaced the old proxy abandons its generation: the old one is current again,
// its tokens still stamp it, and the next revive takes a new number, never the abandoned one.
func TestRunPolicyState_AbandonedGenerationIsNeverReused(t *testing.T) {
	s, gen := dispatched().Advance("d2", []string{"jti-1b", "jti-1a"}, "jti-2")
	s = s.Abandon(gen)
	s.LiveTokenIDs = []string{"jti-1b", "jti-1a"} // the kept proxy's lineage, handed back
	want := PolicyStamp{Digest: "d1", Generation: 1}
	if s.Digest != "d1" || s.Stamp(false, "") != want || s.Stamp(true, "jti-1b") != want {
		t.Errorf("after abandon: digest %q, control plane %+v, old token %+v; want d1 generation 1", s.Digest, s.Stamp(false, ""), s.Stamp(true, "jti-1b"))
	}
	s, gen = s.Advance("d3", []string{"jti-1b", "jti-1a"}, "jti-3")
	if gen != 3 {
		t.Errorf("the revive after an abandoned one took generation %d, want 3", gen)
	}
	s.LiveTokenIDs = []string{"jti-3"}
	if got := s.Stamp(true, "jti-1a"); got != want {
		t.Errorf("old token after the second revive: stamp %+v, want %+v", got, want)
	}
}

// CurrentGeneration is the generation the run's proxy enforces: 1 before any revive, the revive's once
// it replaced the proxy, and the one before it again once it was abandoned.
func TestRunPolicyState_CurrentGeneration(t *testing.T) {
	s := dispatched()
	if got := s.CurrentGeneration(); got != 1 {
		t.Errorf("before any revive: generation %d, want 1", got)
	}
	s, gen := s.Advance("d2", nil, "jti-2")
	if got := s.CurrentGeneration(); got != gen {
		t.Errorf("after a revive: generation %d, want %d", got, gen)
	}
	s, gen = s.Advance("d3", nil, "jti-3")
	if got := s.Abandon(gen).CurrentGeneration(); got != 2 {
		t.Errorf("after generation %d was abandoned: generation %d, want 2", gen, got)
	}
	if got := s.Abandon(gen).Abandon(2).CurrentGeneration(); got != 2 {
		t.Errorf("abandoning a generation that is not the newest changed the current one to %d, want 2", got)
	}
	u, gen := RunPolicyState{}.Advance("d2", nil, "jti-2")
	if got := u.Abandon(gen).CurrentGeneration(); got != 1 {
		t.Errorf("an abandoned revive of a run with no recorded digest: generation %d, want 1", got)
	}
}

// A run dispatched before its digest was recorded has generation 1 with no digest: its rows carry no
// stamp, and the revive's generation is stamped as usual.
func TestRunPolicyState_UnrecordedFirstGeneration(t *testing.T) {
	s, gen := RunPolicyState{}.Advance("d2", []string{"jti-0"}, "jti-2")
	if gen != 2 {
		t.Fatalf("generation %d, want 2", gen)
	}
	if got := s.Stamp(true, "jti-0"); got != (PolicyStamp{}) {
		t.Errorf("old token of an unrecorded policy: stamp %+v, want nothing", got)
	}
	if got := s.Stamp(true, "jti-2"); got != (PolicyStamp{Digest: "d2", Generation: 2}) {
		t.Errorf("fresh token: stamp %+v, want d2 generation 2", got)
	}
}

// The generations round-trip through agent_runs.policy_generations' JSON, and Advance does not write
// into the state it was called on.
func TestRunPolicyState_GenerationsRoundTrip(t *testing.T) {
	base := dispatched()
	s, _ := base.Advance("d2", []string{"jti-1b", "", "jti-1b"}, "jti-2")
	if len(base.Generations) != 0 {
		t.Fatalf("Advance changed its receiver: %+v", base.Generations)
	}
	b, err := json.Marshal(s.Generations)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"generation":1,"digest":"d1","token_ids":["jti-1b"]},{"generation":2,"digest":"d2","token_ids":["jti-2"]}]`
	if string(b) != want {
		t.Errorf("generations = %s, want %s", b, want)
	}
	var back []PolicyGeneration
	if err := json.Unmarshal(b, &back); err != nil || len(back) != 2 || back[1].TokenIDs[0] != "jti-2" {
		t.Errorf("round trip = %+v (%v)", back, err)
	}
}
