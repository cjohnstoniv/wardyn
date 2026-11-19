// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestReplaceProxy_Live_AgentReachesTheNewProxyAtOnce, on a real engine with the real proxy: right after a replace
// of a proxy that was serving the agent, as a live egress edit makes, and again right after a revive of a proxy
// that was stopped, the agent's first request through its wardyn-proxy hosts entry is answered, and the proxy has
// the first proxy's MAC address on the run network. A netstack that ignores a gratuitous ARP (gVisor) would
// otherwise keep sending to the old MAC until its neighbour entry ages out, up to about a minute. It runs at each
// of CC1 and CC2 the daemon offers, and WARDYN_TEST_REQUIRE_CLASS names a class that must be offered.
func TestReplaceProxy_Live_AgentReachesTheNewProxyAtOnce(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live proxy replace test")
	}
	ctx := context.Background()
	d, err := New(Config{ProxyImage: liveProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	cs, err := d.Classes(ctx)
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if want := types.ConfinementClass(os.Getenv("WARDYN_TEST_REQUIRE_CLASS")); want != "" && !slices.Contains(cs.Classes, want) {
		t.Fatalf("WARDYN_TEST_REQUIRE_CLASS=%s but the daemon offers %v: is its runtime registered with dockerd?", want, cs.Classes)
	}
	cpURL, cpCA := liveRenewingControlPlane(ctx, t, d)
	for _, class := range []types.ConfinementClass{types.CC1, types.CC2} {
		if !slices.Contains(cs.Classes, class) {
			continue
		}
		t.Run(string(class), func(t *testing.T) {
			spec := runner.SandboxSpec{
				RunID:            uuid.New(),
				Image:            "busybox:latest",
				ConfinementClass: class,
				ProxyConfig: runner.ProxyConfig{
					RunToken:          "replace-test",
					ControlPlaneURL:   cpURL,
					ControlPlaneCAPEM: cpCA,
					Policy:            types.RunPolicySpec{MinConfinementClass: class},
				},
				Labels: map[string]string{"wardyn.test": "proxy-replace"},
			}
			sb, err := d.CreateSandbox(ctx, spec)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
			via := fmt.Sprintf("http://wardyn-proxy:%d", runner.ProxyListenPort)
			if ok, out := proxyAnswered(ctx, t, d, sb.Ref, via); !ok {
				t.Fatalf("before the replace, the agent got no answer from its proxy:\n%s", out)
			}
			first := liveProxyMAC(ctx, t, d, spec.RunID)
			cfg, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				name string
				stop bool
			}{
				{"a replace of the serving proxy", false},
				{"a revive of the stopped proxy", true},
			} {
				if step.stop {
					if err := d.StopProxy(ctx, sb.Ref); err != nil {
						t.Fatalf("StopProxy: %v", err)
					}
				}
				if err := d.ReplaceProxy(ctx, sb.Ref, cfg); err != nil {
					t.Fatalf("%s: ReplaceProxy: %v", step.name, err)
				}
				if ok, out := proxyAnswered(ctx, t, d, sb.Ref, via); !ok {
					t.Errorf("after %s, the agent's first request got no answer from the new proxy:\n%s", step.name, out)
				}
				if got := liveProxyMAC(ctx, t, d, spec.RunID); got != first {
					t.Errorf("after %s, the proxy's MAC on the run network = %s, want the first proxy's %s", step.name, got, first)
				}
			}
		})
	}
}

// liveProxyMAC is the MAC address the engine reports for the run's proxy on its run network.
func liveProxyMAC(ctx context.Context, t *testing.T, d *Driver, runID uuid.UUID) string {
	t.Helper()
	res, err := d.cli.ContainerInspect(ctx, proxyContainerName(runID), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect proxy: %v", err)
	}
	ns := res.Container.NetworkSettings
	if ns == nil || ns.Networks[internalNetName(runID)] == nil || len(ns.Networks[internalNetName(runID)].MacAddress) == 0 {
		t.Fatalf("the proxy reports no MAC address on %s", internalNetName(runID))
	}
	return ns.Networks[internalNetName(runID)].MacAddress.String()
}
