// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestProxySandboxPeersOnly_RealDocker is the default (WARDYN_DOCKER_PROXY_SANDBOX_PEERS_ONLY unset) on a
// real daemon with the real wardyn-proxy image (WARDYN_TEST_PROXY_IMAGE, a build of this tree): a caller on
// the run's own network gets an HTTP answer from the run's proxy, and a caller on the control-plane network
// the proxy also joins is closed without one.
func TestProxySandboxPeersOnly_RealDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker proxy peers test")
	}
	img := os.Getenv("WARDYN_TEST_PROXY_IMAGE")
	if img == "" {
		t.Skip("set WARDYN_TEST_PROXY_IMAGE to a wardyn-proxy build of this tree")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cpNet := "wardyn-test-cp-" + uuid.NewString()[:8]
	d, err := New(Config{ProxyImage: img, InternalNetwork: cpNet})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, cpNet)
	t.Cleanup(func() { _, _ = d.cli.NetworkRemove(context.Background(), cpNet, client.NetworkRemoveOptions{}) })

	runID := uuid.New()
	sb, err := d.CreateSandbox(ctx, runner.SandboxSpec{
		RunID:            runID,
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig: runner.ProxyConfig{
			RunToken:        "run-token-" + uuid.NewString(),
			ControlPlaneURL: "http://127.0.0.1:9",
			Policy:          types.RunPolicySpec{AllowedDomains: []string{"example.com"}},
		},
		Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 128},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	name := proxyContainerName(runID)
	waitForLog(ctx, t, name, "listening")

	res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect proxy: %v", err)
	}
	ipOn := func(net string) string {
		ep := res.Container.NetworkSettings.Networks[net]
		if ep == nil || !ep.IPAddress.IsValid() {
			t.Fatalf("proxy has no address on %s", net)
		}
		return ep.IPAddress.String()
	}
	runNet := internalNetName(runID)
	peer := probeProxy(ctx, t, d, runNet, ipOn(runNet))
	t.Logf("peer on the run's network %s: %q", runNet, firstLine(peer))
	if !strings.Contains(peer, "HTTP/1.") {
		t.Errorf("a peer on the run's network got %q from the proxy; want an HTTP answer", peer)
	}
	other := probeProxy(ctx, t, d, cpNet, ipOn(cpNet))
	t.Logf("non-peer on the control-plane network %s: %q", cpNet, firstLine(other))
	if strings.Contains(other, "HTTP/") {
		t.Errorf("a caller on the control-plane network got %q from the proxy; want the connection closed unanswered", other)
	}
}

// probeProxy sends one plain-HTTP proxy request to ip:ProxyListenPort from a busybox container on net and
// returns what came back.
func probeProxy(ctx context.Context, t *testing.T, d *Driver, net, ip string) string {
	t.Helper()
	req := `printf 'GET http://example.com/ HTTP/1.0\r\n\r\n' | nc -w 5 ` + ip + " " + strconv.Itoa(runner.ProxyListenPort) + "; true"
	resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: "busybox:latest", Cmd: []string{"sh", "-c", req}},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(net)},
		Name:       "wardyn-test-peer-probe-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("create probe on %s: %v", net, err)
	}
	defer func() {
		_, _ = d.cli.ContainerRemove(context.Background(), resp.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := d.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start probe on %s: %v", net, err)
	}
	waitExit(ctx, t, d, resp.ID)
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	rc, err := cli.ContainerLogs(ctx, resp.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("probe logs on %s: %v", net, err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return strings.TrimSpace(l) }
