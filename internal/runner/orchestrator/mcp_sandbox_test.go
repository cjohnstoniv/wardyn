// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpSubstrate is a fakeSubstrate that runs MCP sandboxes of kinds and records what reaches it.
type mcpSubstrate struct {
	*fakeSubstrate
	kinds           []types.MachineKind
	creates, starts []runner.MCPSandboxID
	kills, statuses []runner.MCPSandboxID
	runKills        []uuid.UUID
	mcpProxyStops   []string
}

func (m *mcpSubstrate) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	cs, err := m.fakeSubstrate.Classes(ctx)
	cs.MCPSandboxKinds = m.kinds
	return cs, err
}

func (m *mcpSubstrate) CreateMCPSandbox(_ context.Context, spec runner.MCPSandboxSpec) (runner.MCPSandbox, error) {
	m.creates = append(m.creates, spec.ID)
	return runner.MCPSandbox{ID: spec.ID, RelayAddr: "10.89.255.254:9443"}, nil
}

func (m *mcpSubstrate) StartMCPSandbox(_ context.Context, id runner.MCPSandboxID) error {
	m.starts = append(m.starts, id)
	return nil
}

func (m *mcpSubstrate) KillRunMCPSandboxes(_ context.Context, runID uuid.UUID) error {
	m.runKills = append(m.runKills, runID)
	return nil
}

func (m *mcpSubstrate) KillMCPSandbox(_ context.Context, id runner.MCPSandboxID) error {
	m.kills = append(m.kills, id)
	return nil
}

func (m *mcpSubstrate) MCPSandboxStatus(_ context.Context, id runner.MCPSandboxID) (runner.MCPSandbox, error) {
	m.statuses = append(m.statuses, id)
	return runner.MCPSandbox{ID: id, Live: true}, nil
}

func (m *mcpSubstrate) StopMCPProxies(_ context.Context, ref string) error {
	m.mcpProxyStops = append(m.mcpProxyStops, ref)
	return nil
}

// TestCapabilitiesMCPSandboxKindsIsAnIntersection: the kinds are those every executor runs, so one executor
// without them takes them from the deployment, in either order, and none wired advertises none.
func TestCapabilitiesMCPSandboxKindsIsAnIntersection(t *testing.T) {
	ctx := context.Background()
	container := []types.MachineKind{types.MachineContainer}
	sub := func(name string, kinds []types.MachineKind) substrate.Substrate {
		return &mcpSubstrate{fakeSubstrate: &fakeSubstrate{name: name, classes: []types.ConfinementClass{types.CC1}}, kinds: kinds}
	}
	for _, tc := range []struct {
		name string
		subs []substrate.Substrate
		want []types.MachineKind
	}{
		{"one docker executor", []substrate.Substrate{sub("docker", container)}, container},
		{"two that both run them", []substrate.Substrate{sub("a", container), sub("b", container)}, container},
		{"one without them", []substrate.Substrate{sub("docker", container), sub("k8s", nil)}, nil},
		{"order does not matter", []substrate.Substrate{sub("k8s", nil), sub("docker", container)}, nil},
		{"a kind one lacks", []substrate.Substrate{sub("a", []types.MachineKind{types.MachineContainer, types.MachineLinuxVM}), sub("b", container)}, container},
		{"none wired", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps, err := New(tc.subs...).Capabilities(ctx)
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if !slices.Equal(caps.MCPSandboxKinds, tc.want) {
				t.Errorf("MCPSandboxKinds = %v, want %v", caps.MCPSandboxKinds, tc.want)
			}
		})
	}
}

// TestMCPSandboxRoutesByClassAndRef: the sandbox calls reach the substrate the class routes to; a proxy stop
// reaches the ref's substrate; the run-wide kill reaches every substrate that can hold MCP sandboxes; a class no
// MCP-capable substrate enforces is refused, and a ref on a substrate without MCP proxies stops nothing.
func TestMCPSandboxRoutesByClassAndRef(t *testing.T) {
	ctx := context.Background()
	docker := &mcpSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}},
		kinds: []types.MachineKind{types.MachineContainer}}
	plain := &fakeSubstrate{name: "plain", classes: []types.ConfinementClass{types.CC2}}
	o := New(docker, plain)
	id := runner.MCPSandboxID{RunID: uuid.New(), Ordinal: 1, Class: types.CC1}
	if sb, err := o.CreateMCPSandbox(ctx, runner.MCPSandboxSpec{ID: id}); err != nil || sb.RelayAddr == "" {
		t.Fatalf("CreateMCPSandbox = %+v, %v", sb, err)
	}
	if err := o.StartMCPSandbox(ctx, id); err != nil {
		t.Fatalf("StartMCPSandbox: %v", err)
	}
	if st, err := o.MCPSandboxStatus(ctx, id); err != nil || !st.Live {
		t.Fatalf("MCPSandboxStatus = %+v, %v", st, err)
	}
	if err := o.KillMCPSandbox(ctx, id); err != nil {
		t.Fatalf("KillMCPSandbox: %v", err)
	}
	if err := o.KillRunMCPSandboxes(ctx, id.RunID); err != nil {
		t.Fatalf("KillRunMCPSandboxes: %v", err)
	}
	for name, got := range map[string][]runner.MCPSandboxID{"creates": docker.creates, "starts": docker.starts, "statuses": docker.statuses, "kills": docker.kills} {
		if !slices.Equal(got, []runner.MCPSandboxID{id}) {
			t.Errorf("%s = %v, want [%v]", name, got, id)
		}
	}
	if !slices.Equal(docker.runKills, []uuid.UUID{id.RunID}) {
		t.Errorf("run kills = %v", docker.runKills)
	}
	if _, err := o.CreateMCPSandbox(ctx, runner.MCPSandboxSpec{ID: runner.MCPSandboxID{RunID: id.RunID, Class: types.CC2}}); err == nil {
		t.Error("a class routed to a substrate without MCP sandboxes was accepted")
	}
	sb, err := o.CreateSandbox(ctx, runner.SandboxSpec{RunID: id.RunID, ConfinementClass: types.CC1})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := o.StopMCPProxies(ctx, sb.Ref); err != nil || !slices.Equal(docker.mcpProxyStops, []string{sb.Ref}) {
		t.Errorf("StopMCPProxies = %v, stops %v; want the ref forwarded", err, docker.mcpProxyStops)
	}
	other, err := o.CreateSandbox(ctx, runner.SandboxSpec{RunID: uuid.New(), ConfinementClass: types.CC2})
	if err != nil {
		t.Fatalf("CreateSandbox on plain: %v", err)
	}
	if err := o.StopMCPProxies(ctx, other.Ref); err != nil {
		t.Errorf("StopMCPProxies on a substrate without MCP proxies = %v, want nil", err)
	}
}
