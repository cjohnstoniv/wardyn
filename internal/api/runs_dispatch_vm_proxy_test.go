// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Dispatch never marks a proxy config VM, for a desktop VM run either: only the runner that relays
// the guest through wardyn-vmnet knows the proxy's one client is the relay, and a Kubernetes VM pod
// has a container run's network. No vm key reaches the config dispatch builds.
func TestDispatch_NeverSetsProxyConfigVM(t *testing.T) {
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, ""} {
		fr := &fakeRunner{}
		srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
		run.MachineKind = kind
		st.run = run
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest",
			Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
		})
		fr.mu.Lock()
		pc, calls := fr.lastSpec.ProxyConfig, fr.createCalls
		fr.mu.Unlock()
		if calls != 1 {
			t.Fatalf("machine_kind %q: CreateSandbox calls = %d (hint %q), want 1", kind, calls, st.FailureHint())
		}
		if pc.VM {
			t.Errorf("machine_kind %q: dispatch set ProxyConfig.VM", kind)
		}
		raw, err := runner.BuildProxyConfig(run.ID, pc, runner.ProxyListenPort)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"vm":`) {
			t.Errorf("machine_kind %q: dispatch's proxy config carries the vm key", kind)
		}
	}
}

// The proxy config dispatch stores, and the spec revive rebuilds from it, carry no VM either.
func TestRevive_StoredProxyConfigNeverVM(t *testing.T) {
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, ""} {
		srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
		runnerID := uuid.New()
		run.RunnerID, run.MachineKind = &runnerID, kind
		ls.dispatchTestStore.run, ls.dispatchTestStore.state = run, types.RunPending
		// Keeps the stored row past the runner-lease failure (see dispatchInternalHostsRun).
		srv.cfg.Store = siteConfigDispatchStore{localDispatchStore: ls, sc: &types.SiteConfig{}}
		srv.cfg.ControlPlaneURL = "http://127.0.0.1:9" // LoadConfigBytes takes plain http only to loopback
		ctx := context.Background()
		srv.dispatchRun(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest", Interactive: true,
			Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
		})
		raw, err := srv.loadRunProxyConfig(ctx, run.ID)
		if err != nil {
			t.Fatalf("machine_kind %q: no stored proxy config (hint %q): %v", kind, ls.dispatchTestStore.FailureHint(), err)
		}
		if strings.Contains(string(raw), `"vm":`) {
			t.Errorf("machine_kind %q: the stored proxy config carries the vm key", kind)
		}
		cfg, err := proxy.LoadConfigBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		if localStoredSpec(*cfg).ProxyConfig.VM {
			t.Errorf("machine_kind %q: the revived spec's ProxyConfig.VM is set", kind)
		}
	}
}
