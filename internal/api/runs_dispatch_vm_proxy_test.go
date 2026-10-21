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

// Dispatch marks a desktop VM run's proxy config VM, so its proxy keeps the port rule on and
// honours wardyn-vmnet's contract; a container run's config is unchanged and carries no vm key.
func TestDispatch_ProxyConfigVMFollowsMachineKind(t *testing.T) {
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
		if pc.VM != kind.IsVM() {
			t.Errorf("machine_kind %q: ProxyConfig.VM = %v, want %v", kind, pc.VM, kind.IsVM())
		}
		raw, err := runner.BuildProxyConfig(run.ID, pc, runner.ProxyListenPort)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(raw), `"vm":`); got != kind.IsVM() {
			t.Errorf("machine_kind %q: proxy config carries the vm key = %v, want %v", kind, got, kind.IsVM())
		}
	}
}

// Revive rebuilds a local run's spec from the proxy config dispatch stored: a VM run's proxy
// comes back VM, a container run's does not.
func TestRevive_StoredProxyConfigKeepsVM(t *testing.T) {
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, ""} {
		srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
		runnerID := uuid.New()
		run.RunnerID, run.MachineKind = &runnerID, kind
		ls.dispatchTestStore.run, ls.dispatchTestStore.state = run, types.RunPending
		// Keeps the stored row past the runner-lease failure (see dispatchInternalHostsRun).
		srv.cfg.Store = siteConfigDispatchStore{localDispatchStore: ls, sc: &types.SiteConfig{}}
		ctx := context.Background()
		srv.dispatchRun(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest", Interactive: true,
			Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
		})
		raw, err := srv.loadRunProxyConfig(ctx, run.ID)
		if err != nil {
			t.Fatalf("machine_kind %q: no stored proxy config (hint %q): %v", kind, ls.dispatchTestStore.FailureHint(), err)
		}
		cfg, err := proxy.LoadConfigBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.VM != kind.IsVM() {
			t.Errorf("machine_kind %q: stored proxy config VM = %v, want %v", kind, cfg.VM, kind.IsVM())
		}
		if got := localStoredSpec(*cfg).ProxyConfig.VM; got != kind.IsVM() {
			t.Errorf("machine_kind %q: revived ProxyConfig.VM = %v, want %v", kind, got, kind.IsVM())
		}
	}
}
