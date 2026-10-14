// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestCreateRunRequest_PlacementIsThePlacementPackage: the request's Placement
// is placement.Placement (a type ALIAS, so the two assign without conversion)
// and PlacementRequest hands the placement package exactly what the wire carried,
// so the shape check a request goes through is the one it was validated against.
func TestCreateRunRequest_PlacementIsThePlacementPackage(t *testing.T) {
	var req client.CreateRunRequest
	req.Placement = placement.Local // compiles only because client.Placement IS placement.Placement
	req.RunnerID = "11111111-1111-1111-1111-111111111111"

	got := req.PlacementRequest()
	if got.Placement != placement.Local || got.RunnerID != req.RunnerID {
		t.Fatalf("PlacementRequest = %+v, want the request's own placement and runner", got)
	}
	if client.PlacementRemote != placement.Remote || client.PlacementLocal != placement.Local {
		t.Fatal("client's placement constants are not placement's")
	}
	// A runner id is accepted only with local placement (placement.Request.Validate).
	if err := got.Validate(); err != nil {
		t.Errorf("a local run naming a UUID runner: %v", err)
	}
	req.Placement = placement.Remote
	if err := req.PlacementRequest().Validate(); err == nil {
		t.Error("a remote placement carrying a runner_id validated")
	}
}

// TestCreateRunRequest_PlacementWireRoundTrip: the two fields are exactly the
// placement package's tags, and a request that names neither omits both keys —
// an absent placement is a choice ("you decide"), not an explicit "remote".
func TestCreateRunRequest_PlacementWireRoundTrip(t *testing.T) {
	b, err := json.Marshal(client.CreateRunRequest{Agent: "claude-code", Repo: "acme/r"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"placement", "runner_id"} {
		if _, present := raw[k]; present {
			t.Errorf("a request naming no placement posts %q; both fields must be omitempty", k)
		}
	}

	in := client.CreateRunRequest{Agent: "claude-code", Repo: "acme/r",
		Placement: client.PlacementLocal, RunnerID: "22222222-2222-2222-2222-222222222222"}
	b, err = json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out client.CreateRunRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Placement != client.PlacementLocal || out.RunnerID != in.RunnerID {
		t.Errorf("round trip = %q/%q, want %q/%q", out.Placement, out.RunnerID, in.Placement, in.RunnerID)
	}
}
