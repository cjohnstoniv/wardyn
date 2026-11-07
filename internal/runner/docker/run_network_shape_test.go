// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// TestRunNetwork_CreatedWithNoHostAddress: on Docker Engine 28 or later, a run's network is created
// in gateway mode "isolated" with IPv6 off, whichever way its subnet is picked.
func TestRunNetwork_CreatedWithNoHostAddress(t *testing.T) {
	for name, pool := range map[string]string{"daemon-picked subnet": "", "configured pool": "10.200.0.0/24"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			d := newTestDriver(f)
			if pool != "" {
				d = newPoolDriver(f, pool)
			}
			var creates []client.NetworkCreateOptions
			f.onNetworkCreate = func(_ string, opts client.NetworkCreateOptions) { creates = append(creates, opts) }
			if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			if len(creates) == 0 {
				t.Fatal("no network create")
			}
			for _, opts := range creates {
				if opts.Options[bridgeGatewayModeIPv4] != gatewayModeIsolated || opts.EnableIPv6 == nil || *opts.EnableIPv6 || !opts.Internal {
					t.Errorf("run network create = %+v; want internal, gateway mode isolated and IPv6 off", opts)
				}
			}
		})
	}
}

// TestRunNetwork_OldEngineRefusesTheRun: an engine older than 28.0.0 (or one whose version does not
// read) refuses the run before any network exists, naming the version and the override; with the
// override the run starts on a network without the mode.
func TestRunNetwork_OldEngineRefusesTheRun(t *testing.T) {
	for _, version := range []string{"27.5.1", "", "dev"} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		f.info.ServerVersion = version
		_, err := newTestDriver(f).CreateSandbox(context.Background(), testSpec())
		if err == nil || !strings.Contains(err.Error(), "Docker Engine 28.0.0 or later") || !strings.Contains(err.Error(), EnvAllowHostReachableRunNetwork) {
			t.Fatalf("engine %q: CreateSandbox = %v; want a refusal naming Docker Engine 28.0.0 and %s", version, err, EnvAllowHostReachableRunNetwork)
		}
		if _, ok := f.networks[internalNetName(testSpec().RunID)]; ok {
			t.Errorf("engine %q: a refused run left its network behind", version)
		}
	}

	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.info.ServerVersion = "27.5.1"
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", AllowHostReachableRunNetwork: true})
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox with the override: %v", err)
	}
	if opts := f.networks[internalNetName(testSpec().RunID)]; opts.Options[bridgeGatewayModeIPv4] != "" {
		t.Errorf("run network on engine 27 with the override = %+v; want no gateway mode, which that engine refuses", opts)
	}
}

// TestRunNetwork_KeptFromBeforeIsRefused: a run whose network was created without gateway mode
// "isolated" (before the upgrade) is neither revived nor started again, and its proxy is left running;
// the override accepts it.
func TestRunNetwork_KeptFromBeforeIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, allow := range []bool{false, true} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", AllowHostReachableRunNetwork: allow})
		sb, err := d.CreateSandbox(ctx, testSpec())
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		name := internalNetName(testSpec().RunID)
		f.mu.Lock()
		old := f.networks[name]
		old.Options = nil // the shape a run network had before the upgrade
		f.networks[name] = old
		f.mu.Unlock()

		if allow {
			// The kept-agent path: the agent and proxy ended, then a new proxy and the agent started again.
			if err := d.EndSandbox(ctx, sb.Ref); err != nil {
				t.Fatalf("EndSandbox: %v", err)
			}
			if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
				t.Errorf("ReplaceProxy with the override = %v; want it accepted", err)
			}
			if err := d.StartSandbox(ctx, sb.Ref); err != nil {
				t.Errorf("StartSandbox with the override = %v; want it accepted", err)
			}
			continue
		}
		replaceErr := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh"))
		if p := f.containers[proxyContainerName(testSpec().RunID)]; p == nil || p.removed {
			t.Errorf("proxy after the refused revive = %+v; want the running one kept", p)
		}
		startErr := d.StartSandbox(ctx, sb.Ref)
		for what, err := range map[string]error{"ReplaceProxy": replaceErr, "StartSandbox": startErr} {
			if err == nil || !strings.Contains(err.Error(), "address on the Docker host") || !strings.Contains(err.Error(), "start a new run") {
				t.Errorf("%s on a network from before the upgrade = %v; want a refusal saying to start a new run", what, err)
			}
		}
	}
}

func TestEngineMajor(t *testing.T) {
	for in, want := range map[string]int{"29.4.2": 29, "28.0.0-rc.1": 28, " 28.5.1+dfsg1": 28, "27.5.1": 27, "": 0, "dev": 0, "v28.0.0": 0} {
		if got := engineMajor(in); got != want {
			t.Errorf("engineMajor(%q) = %d; want %d", in, got, want)
		}
	}
}
