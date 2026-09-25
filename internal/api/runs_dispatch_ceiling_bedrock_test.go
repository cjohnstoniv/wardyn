// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bedrockAWSDirCfg is the HOST-MODE Bedrock posture: no stored credential of
// any kind, an existing ~/.aws to bind read-only. The residual risk is
// sharpest here — the operator's whole AWS config directory in the sandbox of a
// principal whose profile denies Bedrock outright.
func bedrockAWSDirCfg(t *testing.T) Config {
	t.Helper()
	return Config{
		BedrockRegion:       "us-east-1",
		BedrockModel:        "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		BedrockAWSConfigDir: t.TempDir(), // a real, existing dir so the os.Stat fail-safe passes
		BedrockAWSProfile:   "bedrock-sso",
		Secrets:             &memSecrets{m: map[string][]byte{}},
		MaskRegistry:        secretmask.NewRegistry(),
	}
}

// ceilingBedrockFixture resolves a REAL Bedrock transport for a run (so the
// sandbox env, the secretEnvKeys and the mount decision are the ones dispatch
// would carry) and returns everything the re-assertion phase reads.
func ceilingBedrockFixture(t *testing.T, cfg Config) (*Server, *recRecorder, types.AgentRun, llmTransport, map[string]string, *types.RunPolicySpec) {
	t.Helper()
	h := newHarness(t)
	audit := &recRecorder{}
	cfg.Identity, cfg.Audit = h.idp, audit
	cfg.TrustDomain, cfg.ControlPlaneURL = "wardyn.local", "http://wardynd:8080"
	srv := New(cfg)

	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: "sub-member@corp.example", State: types.RunStarting}
	policy := &types.RunPolicySpec{MinConfinementClass: types.CC2}
	sandboxEnv := map[string]string{}
	llm := srv.resolveLLMTransport(context.Background(), run, policy, sandboxEnv, nil,
		false, "", "http://wardyn-proxy:3128", nil, awsSSOScope{})
	if !llm.bedrockReady {
		t.Fatal("fixture did not resolve a ready Bedrock lane; there is nothing for the ceiling to withhold")
	}
	return srv, audit, run, llm, sandboxEnv, policy
}

// The re-assertion dropped the Bedrock BEARER injection — a bearer rides an
// injection rule and injection rules are filtered by host — but the bearer is
// the one Bedrock mode that is never resident. A profile denying the Bedrock
// hosts left the RESIDENT SigV4 keys in the sandbox env, still named by
// llm.secretEnvKeys (so splitSecretEnv moved them onto SandboxSpec.SecretEnv),
// and left the operator's whole host ~/.aws bind-mounted read-only. The proxy
// still denied the host, so this is credential RESIDENCY in a sandbox whose
// principal is denied the service those credentials are for.
func TestCeilingReassert_WithholdsTheResidentBedrockLane(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) Config{
		"static SigV4 keys in the sandbox env": func(*testing.T) Config { return bedrockStaticCfg() },
		"the operator's host ~/.aws mount":     bedrockAWSDirCfg,
	} {
		t.Run(name, func(t *testing.T) {
			srv, audit, run, llm, sandboxEnv, policy := ceilingBedrockFixture(t, mk(t))
			hosts := slices.Clone(llm.bedrock.egressHosts)
			if len(hosts) == 0 {
				t.Fatal("the resolved Bedrock lane names no egress hosts to deny")
			}
			c := dispatchCeiling{resolved: true, deny: hosts, profile: "walled"}
			p := dispatchParams{}
			var injections []runner.InjectionGrant
			mitm := []string{"bedrock-runtime.us-east-1.amazonaws.com:443"}

			srv.reassertCeilingDenies(context.Background(), run, policy, &injections, c, &p, sandboxEnv, &llm, &mitm)

			for k := range sandboxEnv {
				if strings.HasPrefix(k, "AWS_") {
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
			for _, m := range buildRunMounts(*policy, llm, memberMountPosture{}) {
				if m.Target == sandboxAWSDir {
					t.Errorf("the operator's host ~/.aws is still bind-mounted at %s", m.Target)
				}
			}
			if lanes := reassertDroppedLanes(t, audit); !slices.Contains(lanes, bedrockCeilingLane) {
				t.Errorf("dropped_broker_lanes = %v, want the withheld %q lane named", lanes, bedrockCeilingLane)
			}
		})
	}
}

// TestCeilingReassert_NoProfileLeavesBedrockAlone is the negative control:
// with no assigned profile the phase must be a provable no-op, so the sandbox
// env, the transport and the mounts are byte-identical to what dispatch
// composed.
func TestCeilingReassert_NoProfileLeavesBedrockAlone(t *testing.T) {
	srv, _, run, llm, sandboxEnv, policy := ceilingBedrockFixture(t, bedrockStaticCfg())
	before := make(map[string]string, len(sandboxEnv))
	for k, v := range sandboxEnv {
		before[k] = v
	}
	keysBefore := slices.Clone(llm.secretEnvKeys)
	mountsBefore := buildRunMounts(*policy, llm, memberMountPosture{})

	var injections []runner.InjectionGrant
	p := dispatchParams{}
	mitm := []string{"bedrock-runtime.us-east-1.amazonaws.com:443"}
	srv.reassertCeilingDenies(context.Background(), run, policy, &injections, dispatchCeiling{resolved: true},
		&p, sandboxEnv, &llm, &mitm)

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
	if len(buildRunMounts(*policy, llm, memberMountPosture{})) != len(mountsBefore) {
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
