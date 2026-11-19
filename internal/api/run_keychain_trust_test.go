// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A macOS VM whose guest could not trust the run's proxy CA gets one denied run.dispatch row with the
// fixed codes and the run goes on; a trusted CA and an absent answer (not attempted) write none, and a
// code the host does not know is written as unavailable, never as the runner sent it.
func TestAuditKeychainTrust(t *testing.T) {
	for _, tc := range []struct {
		name       string
		k          *runner.KeychainTrust
		wantReason string // "" = no row
	}{
		{"ok", &runner.KeychainTrust{Status: runner.KeychainTrusted}, ""},
		{"failed", &runner.KeychainTrust{Status: runner.KeychainNotTrusted, Reason: runner.KeychainRefused}, runner.KeychainRefused},
		{"absent", nil, ""},
		{"unknown", &runner.KeychainTrust{Status: runner.KeychainNotTrusted, Reason: "guest text"}, runner.KeychainUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVMSignInFixture(t, "run_disk")
			mac := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineMacOSVM}
			f.srv.auditKeychainTrust(context.Background(), mac, runner.Sandbox{Ref: "vm-1", KeychainTrust: tc.k})
			var rows []types.AuditEvent
			for _, ev := range f.h.audit.snapshot() {
				if ev.Action == "run.dispatch" && ev.RunID != nil && *ev.RunID == mac.ID {
					rows = append(rows, ev)
				}
			}
			if tc.wantReason == "" {
				if len(rows) != 0 {
					t.Fatalf("rows = %+v, want none", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Outcome != "denied" {
				t.Fatalf("rows = %+v, want one denied row", rows)
			}
			var d map[string]any
			if err := json.Unmarshal(rows[0].Data, &d); err != nil {
				t.Fatal(err)
			}
			if d["keychain_trust"] != runner.KeychainNotTrusted || d["keychain_trust_reason"] != tc.wantReason || d["note"] != keychainTrustNote || len(d) != 3 {
				t.Errorf("data = %v", d)
			}
		})
	}
}

// A No network run on Kubernetes whose namespace was not proven enforced gets one denied run.dispatch row
// naming the proxy layer only and why; "pod_enforced" gets none. An empty answer from a Kubernetes run (a
// driver or runner from before the field) is "not_proven", and from any other run writes nothing. A value the
// host does not know is written as "not_proven", never as the runner sent it. A run whose preset is not No
// network (another preset, or none) gets no row for any answer, as the row speaks of a No network run.
// Catches a run under an override that the audit cannot tell apart, an older runner's silence read as proven,
// and a runner's field written as a claim about a run that is not No network.
func TestDispatch_NoNetworkProxyOnlyIsRecorded(t *testing.T) {
	noNetwork := &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network", Scope: types.WallPresetScopeBuiltin}
	other := &types.WallPresetOnRun{ID: "other", Name: "Other", Scope: types.WallPresetScopeBuiltin}
	f := newVMSignInFixture(t, "run_disk")
	f.srv.cfg.RunnerTarget = "k8s"
	kube, plain := uuid.New(), uuid.New()
	seedRunnerCaps(t, f.srv, kube, runnerwire.Caps{EndsUnkept: true})
	seedRunnerCaps(t, f.srv, plain, runnerwire.Caps{MachineName: "build-mac"})
	for _, tc := range []struct {
		name     string
		code     string
		run      types.AgentRun
		runner   *uuid.UUID
		target   string
		wantCode string // "" = no row
	}{
		{"not enforced", "netpol_not_enforced", types.AgentRun{WallPreset: noNetwork}, nil, "k8s", "netpol_not_enforced"},
		{"acknowledged", "netpol_ambient_acknowledged", types.AgentRun{WallPreset: noNetwork}, nil, "k8s", "netpol_ambient_acknowledged"},
		{"not proven", "netpol_not_proven", types.AgentRun{WallPreset: noNetwork}, nil, "k8s", "netpol_not_proven"},
		{"enforced", "pod_enforced", types.AgentRun{WallPreset: noNetwork}, nil, "k8s", ""},
		{"unknown text", "the runner's own words", types.AgentRun{WallPreset: noNetwork}, nil, "k8s", "not_proven"},
		{"empty, Kubernetes server", "", types.AgentRun{WallPreset: noNetwork}, nil, "k8s", "not_proven"},
		{"empty, runner that ends unkept", "", types.AgentRun{WallPreset: noNetwork}, &kube, "docker", "not_proven"},
		{"empty, Docker server", "", types.AgentRun{WallPreset: noNetwork}, nil, "docker", ""},
		{"empty, runner that keeps its sandbox", "", types.AgentRun{WallPreset: noNetwork}, &plain, "k8s", ""},
		{"empty, another preset", "", types.AgentRun{WallPreset: other}, nil, "k8s", ""},
		{"empty, no preset", "", types.AgentRun{}, nil, "k8s", ""},
		{"not enforced, another preset", "netpol_not_enforced", types.AgentRun{WallPreset: other}, nil, "k8s", ""},
		{"acknowledged, another preset", "netpol_ambient_acknowledged", types.AgentRun{WallPreset: other}, nil, "k8s", ""},
		{"not proven, another preset", "netpol_not_proven", types.AgentRun{WallPreset: other}, nil, "k8s", ""},
		{"enforced, another preset", "pod_enforced", types.AgentRun{WallPreset: other}, nil, "k8s", ""},
		{"unknown text, another preset", "the runner's own words", types.AgentRun{WallPreset: other}, nil, "k8s", ""},
		{"not enforced, no preset", "netpol_not_enforced", types.AgentRun{}, nil, "k8s", ""},
		{"acknowledged, no preset", "netpol_ambient_acknowledged", types.AgentRun{}, nil, "k8s", ""},
		{"not proven, no preset", "netpol_not_proven", types.AgentRun{}, nil, "k8s", ""},
		{"enforced, no preset", "pod_enforced", types.AgentRun{}, nil, "k8s", ""},
		{"unknown text, no preset", "the runner's own words", types.AgentRun{}, nil, "k8s", ""},
		{"unknown text, another preset, runner that ends unkept", "the runner's own words", types.AgentRun{WallPreset: other}, &kube, "docker", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.srv.cfg.RunnerTarget = tc.target
			run := tc.run
			run.ID, run.RunnerID = uuid.New(), tc.runner
			f.srv.auditNoNetworkProxyOnly(context.Background(), run, runner.Sandbox{Ref: "pod-1", NoNetworkProxyOnly: tc.code})
			var rows []types.AuditEvent
			for _, ev := range f.h.audit.snapshot() {
				if ev.Action == "run.dispatch" && ev.RunID != nil && *ev.RunID == run.ID {
					rows = append(rows, ev)
				}
			}
			if tc.wantCode == "" {
				if len(rows) != 0 {
					t.Fatalf("rows = %+v, want none", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Outcome != "denied" {
				t.Fatalf("rows = %+v, want one denied row", rows)
			}
			if strings.Contains(string(rows[0].Data), "own words") {
				t.Errorf("the runner's text reached the row: %s", rows[0].Data)
			}
			var d map[string]any
			if err := json.Unmarshal(rows[0].Data, &d); err != nil {
				t.Fatal(err)
			}
			if d["no_network_isolation"] != "proxy_only" || d["network_policy"] != tc.wantCode || d["note"] != noNetworkProxyOnlyNote || len(d) != 3 {
				t.Errorf("data = %v", d)
			}
		})
	}
}
