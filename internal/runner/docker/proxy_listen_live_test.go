// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// liveProxyImage is the real sidecar, built by `make agent-images` / compose.
const liveProxyImage = "wardyn/wardyn-proxy:local"

// proxyAnswered runs a proxied GET from ref through proxyURL and reports
// whether an HTTP response came back (any status: the proxy answered).
func proxyAnswered(ctx context.Context, t *testing.T, d *Driver, ref, proxyURL string) (bool, string) {
	t.Helper()
	out := execInSandbox(ctx, t, d, ref, []string{"sh", "-c",
		"http_proxy=" + proxyURL + " wget -S -O /dev/null -T 5 http://listen-probe.invalid/ 2>&1; true"})
	return strings.Contains(out, "HTTP/1."), out
}

// TestProxyListener_Live_AgentOnlyOnRunNetwork, on a real engine with the real
// proxy: the agent reaches its proxy through its wardyn-proxy hosts entry,
// and a container on the control-plane-facing network gets no answer from
// the proxy's address there, both after create and after a replacement.
func TestProxyListener_Live_AgentOnlyOnRunNetwork(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live proxy listener test")
	}
	ctx := context.Background()
	d, err := New(Config{ProxyImage: liveProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)

	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig: runner.ProxyConfig{
			RunToken:        "listen-test",
			ControlPlaneURL: "http://127.0.0.1:9",
			Policy:          types.RunPolicySpec{MinConfinementClass: types.CC1},
		},
		Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 128},
		Labels:    map[string]string{"wardyn.test": "proxy-listen"},
	}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })

	probe, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: "busybox:latest", Cmd: []string{"sleep", "600"}, Labels: spec.Labels},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(d.cfg.InternalNetwork)},
		Name:       "wardyn-listen-probe-" + spec.RunID.String(),
	})
	if err != nil {
		t.Fatalf("create probe: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.cli.ContainerRemove(context.Background(), probe.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := d.cli.ContainerStart(ctx, probe.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start probe: %v", err)
	}

	check := func(t *testing.T) {
		if ok, out := proxyAnswered(ctx, t, d, sb.Ref, fmt.Sprintf("http://wardyn-proxy:%d", runner.ProxyListenPort)); !ok {
			t.Errorf("the agent got no answer from its proxy:\n%s", out)
		}
		res, err := d.cli.ContainerInspect(ctx, proxyContainerName(spec.RunID), client.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("inspect proxy: %v", err)
		}
		ep := res.Container.NetworkSettings.Networks[d.cfg.InternalNetwork]
		if ep == nil || !ep.IPAddress.IsValid() {
			t.Fatalf("proxy has no address on %s", d.cfg.InternalNetwork)
		}
		shared := fmt.Sprintf("http://%s:%d", ep.IPAddress, runner.ProxyListenPort)
		if ok, out := proxyAnswered(ctx, t, d, probe.ID, shared); ok {
			t.Errorf("the proxy answered on %s from the shared network:\n%s", shared, out)
		}
	}
	t.Run("create", check)

	cfg, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, cfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	t.Run("replace", check)
}
