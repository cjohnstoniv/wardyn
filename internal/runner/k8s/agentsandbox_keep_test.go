// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestCheckAgentSandboxKeepNeedsTheBackend(t *testing.T) {
	err := checkAgentSandboxKeep(Config{AgentSandboxKeep: true})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_K8S_AGENT_SANDBOX_KEEP needs WARDYN_K8S_AGENT_SANDBOX on") {
		t.Fatalf("keep without the backend: err = %v, want the refusal", err)
	}
	for _, cfg := range []Config{{}, {AgentSandbox: true}, {AgentSandbox: true, AgentSandboxKeep: true}} {
		if err := checkAgentSandboxKeep(cfg); err != nil {
			t.Errorf("checkAgentSandboxKeep(%+v) = %v, want nil", cfg, err)
		}
	}
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX_KEEP", "true")
	if !buildConfig(substrate.Deps{}).AgentSandboxKeep {
		t.Error("buildConfig did not read WARDYN_K8S_AGENT_SANDBOX_KEEP")
	}
	// A runner refuses the switch alone with the keep message, and with the backend with the backend's.
	t.Setenv("WARDYN_K8S_NAMESPACE", "")
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX", "")
	if _, err := NewRunner(substrate.Deps{}, func(*Config) {}); err == nil || !strings.Contains(err.Error(), "WARDYN_K8S_AGENT_SANDBOX_KEEP needs") {
		t.Errorf("NewRunner with keep alone = %v, want the keep refusal", err)
	}
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX", "true")
	if _, err := NewRunner(substrate.Deps{}, func(*Config) {}); !errors.Is(err, errAgentSandboxRunner) {
		t.Errorf("NewRunner with keep and the backend = %v, want errAgentSandboxRunner", err)
	}
}

// Boot refuses a service account missing any verb the kept run's address needs, naming it.
func TestBootKeepChecksTheAddressVerbs(t *testing.T) {
	for _, tc := range []struct{ deny, want string }{
		{"create services", "needs create on services"},
		{"delete services", "needs delete on services"},
		{"update endpointslices", "needs update on endpointslices.discovery.k8s.io"},
		{"deletecollection endpointslices", "needs deletecollection on endpointslices.discovery.k8s.io"},
	} {
		t.Run(tc.deny, func(t *testing.T) {
			cs := fake.NewClientset()
			installCanaryReactor(t, cs, false)
			cs.Resources = servedSandboxes()
			installAccessReactor(cs, tc.deny, nil)
			useDynamicClient(t, newFakeDynamic())
			cfg := Config{AgentSandbox: true, AgentSandboxKeep: true, Namespaces: []string{testNamespace}, ProxyImage: "p"}
			if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newWithClient err = %v, want a refusal containing %q", err, tc.want)
			}
			// With keep off the same denial does not matter.
			cfg.AgentSandboxKeep = false
			if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); err != nil {
				t.Fatalf("keep off: newWithClient err = %v, want boot", err)
			}
		})
	}
}

func TestKeepRunNeedsInteractiveAndAKeptWorkFolder(t *testing.T) {
	work := types.DriveMount{Target: runner.ScratchWorkPath}
	other := types.DriveMount{Target: "/home/agent/other"}
	on := &Driver{cfg: Config{AgentSandbox: true, AgentSandboxKeep: true}}
	off := &Driver{cfg: Config{AgentSandbox: true}}
	for _, tc := range []struct {
		name string
		d    *Driver
		spec runner.SandboxSpec
		want bool
	}{
		{"keep off", off, runner.SandboxSpec{Interactive: true, Drives: []types.DriveMount{work}}, false},
		{"not interactive", on, runner.SandboxSpec{Drives: []types.DriveMount{work}}, false},
		{"no drive", on, runner.SandboxSpec{Interactive: true}, false},
		{"a drive at another target", on, runner.SandboxSpec{Interactive: true, Drives: []types.DriveMount{other}}, false},
		{"interactive with the work drive", on, runner.SandboxSpec{Interactive: true, Drives: []types.DriveMount{other, work}}, true},
	} {
		if got := tc.d.keepRun(tc.spec); got != tc.want {
			t.Errorf("%s: keepRun = %v, want %v", tc.name, got, tc.want)
		}
	}
}
