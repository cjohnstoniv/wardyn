// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ceilingBedrockFixture resolves a REAL Bedrock provider transport for a run
// whose captured AWS SSO session is RESIDENT in the sandbox (no proxy-side
// injection), so the sandbox env, the secretEnvKeys and the MITM decision are
// the ones dispatch would carry, and returns everything the re-assertion phase
// reads.
func ceilingBedrockFixture(t *testing.T) (*Server, *recRecorder, types.AgentRun, llmTransport, map[string]string, *types.RunPolicySpec) {
	t.Helper()
	h := newHarness(t)
	audit := &recRecorder{}
	srv := New(Config{
		Identity: h.idp, Audit: audit, TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080",
		Secrets:      &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}},
		MaskRegistry: secretmask.NewRegistry(), Now: func() time.Time { return awsSSOTestFixedNow },
	})
	blob := putAWSSSOBlob(t, srv, awsSSOTestFixedNow.Add(time.Hour))
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: awsSSOTestOwner, State: types.RunStarting}
	policy := &types.RunPolicySpec{MinConfinementClass: types.CC2}
	sandboxEnv := map[string]string{}
	llm := srv.providerBedrockTransport(context.Background(), run, policy, sandboxEnv,
		chosenProvider{provider: awsSSOTestProvider(), owner: awsSSOTestOwner}, blob)
	if !llm.bedrockReady || len(llm.secretEnvKeys) == 0 {
		t.Fatal("fixture did not resolve a ready, resident Bedrock lane; there is nothing for the ceiling to withhold")
	}
	return srv, audit, run, llm, sandboxEnv, policy
}

// The re-assertion dropped the Bedrock BEARER injection — a bearer rides an
// injection rule and injection rules are filtered by host — but the bearer is
// the one Bedrock mode that is never resident. A profile denying the Bedrock
// hosts left the RESIDENT captured session in the sandbox env, still named by
// llm.secretEnvKeys (so splitSecretEnv moved it onto SandboxSpec.SecretEnv).
// The proxy still denied the host, so this is credential RESIDENCY in a
// sandbox whose principal is denied the service that credential is for.
func TestCeilingReassert_WithholdsTheResidentBedrockLane(t *testing.T) {
	srv, audit, run, llm, sandboxEnv, policy := ceilingBedrockFixture(t)
	hosts := slices.Clone(llm.bedrock.egressHosts)
	if len(hosts) == 0 {
		t.Fatal("the resolved Bedrock lane names no egress hosts to deny")
	}
	written := slices.Collect(maps.Keys(llm.bedrock.env))
	if !slices.Contains(written, awsSSOConfigEnvVar) {
		t.Fatalf("the resident lane wrote %v, not the captured session %s", written, awsSSOConfigEnvVar)
	}
	c := dispatchCeiling{resolved: true, deny: hosts, profile: "walled"}
	p := dispatchParams{}
	var injections []runner.InjectionGrant
	mitm := []string{"bedrock-runtime.us-east-1.amazonaws.com:443"}

	srv.reassertCeilingDenies(context.Background(), run, policy, &injections, c, &p, sandboxEnv, &llm, &mitm, nil)

	for _, k := range written {
		if _, still := sandboxEnv[k]; still {
			t.Errorf("sandbox env still carries %s: the run cannot reach Bedrock and holds its credential anyway", k)
		}
	}
	if len(llm.secretEnvKeys) != 0 {
		t.Errorf("secretEnvKeys = %v, want none — splitSecretEnv would still publish them", llm.secretEnvKeys)
	}
	if llm.bedrockReady {
		t.Error("bedrockReady survived the ceiling")
	}
	if len(mitm) != 0 {
		t.Errorf("bedrock MITM hosts = %v, want none for a lane that was withheld", mitm)
	}
	if lanes := reassertDroppedLanes(t, audit); !slices.Contains(lanes, bedrockCeilingLane) {
		t.Errorf("dropped_broker_lanes = %v, want the withheld %q lane named", lanes, bedrockCeilingLane)
	}
}

// TestCeilingReassert_NoProfileLeavesBedrockAlone is the negative control:
// with no assigned profile the phase must be a provable no-op, so the sandbox
// env, the transport and the mounts are byte-identical to what dispatch
// composed.
func TestCeilingReassert_NoProfileLeavesBedrockAlone(t *testing.T) {
	srv, _, run, llm, sandboxEnv, policy := ceilingBedrockFixture(t)
	before := make(map[string]string, len(sandboxEnv))
	for k, v := range sandboxEnv {
		before[k] = v
	}
	keysBefore := slices.Clone(llm.secretEnvKeys)
	mountsBefore := buildRunMounts(*policy, userMountPosture{})

	var injections []runner.InjectionGrant
	p := dispatchParams{}
	mitm := []string{"bedrock-runtime.us-east-1.amazonaws.com:443"}
	srv.reassertCeilingDenies(context.Background(), run, policy, &injections, dispatchCeiling{resolved: true},
		&p, sandboxEnv, &llm, &mitm, nil)

	if len(sandboxEnv) != len(before) {
		t.Fatalf("sandbox env changed: %v -> %v", before, sandboxEnv)
	}
	for k, v := range before {
		if sandboxEnv[k] != v {
			t.Errorf("sandbox env %s = %q, want %q", k, sandboxEnv[k], v)
		}
	}
	if !slices.Equal(llm.secretEnvKeys, keysBefore) {
		t.Errorf("secretEnvKeys = %v, want %v", llm.secretEnvKeys, keysBefore)
	}
	if !llm.bedrockReady || len(mitm) != 1 {
		t.Errorf("bedrockReady=%v mitm=%v; an unassigned principal's run must be untouched", llm.bedrockReady, mitm)
	}
	if len(buildRunMounts(*policy, userMountPosture{})) != len(mountsBefore) {
		t.Error("the mount set changed for a run with no assigned profile")
	}
}

// reassertDroppedLanes pulls dropped_broker_lanes out of the single
// run.ceiling.reassert row.
func reassertDroppedLanes(t *testing.T, audit *recRecorder) []string {
	t.Helper()
	for _, ev := range audit.events {
		if ev.Action != "run.ceiling.reassert" {
			continue
		}
		var data struct {
			DroppedBrokerLanes []string `json:"dropped_broker_lanes"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode run.ceiling.reassert data: %v", err)
		}
		return data.DroppedBrokerLanes
	}
	t.Fatal("no run.ceiling.reassert row: a profile applied and the row must say which walls the run stood inside")
	return nil
}
