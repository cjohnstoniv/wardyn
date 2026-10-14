// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// liveRelayProbeHost is the hosts-file name the canary dials, as the runner's
// own probe does: a name resolved through /etc/hosts, the same lookup a run's
// proxy uses for the control plane.
const liveRelayProbeHost = "wardyn-relay-probe"

// TestRelayOnThisEngine runs the relay reachability mechanism against the
// engine at DOCKER_HOST: a listener on that engine's relay bind, and the
// hardened proxy-image canary on a user-defined bridge reaching it through an
// ExtraHosts entry. Skipped unless WARDYN_TEST_DOCKER=1; WARDYN_TEST_PROXY_IMAGE
// names a proxy image already on the engine.
//
// TODO: drive docker.(*Driver).ProbeRelay instead of the copy of its container
// spec below once it is on this branch.
func TestRelayOnThisEngine(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker relay test")
	}
	image := os.Getenv("WARDYN_TEST_PROXY_IMAGE")
	if image == "" {
		t.Fatal("set WARDYN_TEST_PROXY_IMAGE to a wardyn-proxy image present on this engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	if _, err := cli.ImageInspect(ctx, image); err != nil {
		t.Fatalf("proxy image %s is not on this engine: %v", image, err)
	}

	// Docker Desktop's host-gateway reaches the host's loopback; a native
	// engine's reaches the docker0 gateway, so that is where the relay binds.
	desktop := info.Info.OperatingSystem == "Docker Desktop"
	bind := "127.0.0.1"
	if !desktop {
		insp, err := cli.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
		if err != nil {
			t.Fatalf("inspect the default bridge: %v", err)
		}
		bind = ""
		for _, c := range insp.Network.IPAM.Config {
			if c.Gateway.Is4() {
				bind = c.Gateway.String()
				break
			}
		}
		if bind == "" {
			t.Fatal("the default bridge has no IPv4 gateway")
		}
	}
	t.Logf("engine %q (%s): listen %s target host-gateway", info.Info.OperatingSystem, info.Info.Name, bind)

	// One probe network for the whole test, a plain user-defined bridge like
	// wardyn-internal, removed afterwards.
	netName := "wardyn-relay-live-" + uuid.NewString()
	if _, err := cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		t.Fatalf("create probe network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.NetworkRemove(context.Background(), netName, client.NetworkRemoveOptions{})
	})

	ln, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		t.Fatalf("listen on the relay bind %s: %v", bind, err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	accepted := make(chan struct{}, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()
	drain := func() {
		for len(accepted) > 0 {
			<-accepted
		}
	}

	probe := func(t *testing.T, target string, port uint16) int64 {
		t.Helper()
		code, err := liveRelayProbe(ctx, cli, image, netName, target, port)
		if err != nil {
			t.Fatalf("probe %s:%d: %v", target, port, err)
		}
		return code
	}

	t.Run("host-gateway reaches the listener", func(t *testing.T) {
		drain()
		// Docker Desktop on WSL2 forwards a fresh WSL loopback listener to the
		// host only after a delay (measured 0 to 3 s on this box), so a probe
		// right after listen can fail. The retry is bounded; the first
		// attempts are logged so the delay stays visible.
		start := time.Now()
		for i := 0; ; i++ {
			code := probe(t, "host-gateway", port)
			t.Logf("attempt %d, %s after listen: exit %d", i, time.Since(start).Round(time.Millisecond), code)
			if code == 0 {
				break
			}
			if i == 20 {
				t.Fatalf("canary to host-gateway:%d exited %d, want 0", port, code)
			}
			time.Sleep(250 * time.Millisecond)
		}
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("the canary exited 0 but the listener never accepted: something else answered")
		}
	})

	t.Run("a closed port exits 1", func(t *testing.T) {
		probeLn, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
		if err != nil {
			t.Fatal(err)
		}
		closed := uint16(probeLn.Addr().(*net.TCPAddr).Port)
		_ = probeLn.Close()
		if code := probe(t, "host-gateway", closed); code != 1 {
			t.Fatalf("canary to closed host-gateway:%d exited %d, want 1", closed, code)
		}
	})

	t.Run("a wrong target exits 1", func(t *testing.T) {
		drain()
		if code := probe(t, "192.168.5.2", port); code != 1 {
			t.Fatalf("canary to 192.168.5.2:%d exited %d, want 1", port, code)
		}
		select {
		case <-accepted:
			t.Fatal("the listener accepted a connection from the wrong-target probe")
		default:
		}
	})
}

// liveRelayProbe runs one hardened proxy-image canary on netName with
// host liveRelayProbeHost mapped to target, and returns its exit code. The
// spec mirrors the runner's relay probe: explicit wardyn-proxy entrypoint,
// every capability dropped, read-only root, no mounts and no environment.
func liveRelayProbe(ctx context.Context, cli *client.Client, image, netName, target string, port uint16) (int64, error) {
	pids := int64(64)
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "wardyn-relay-probe-" + uuid.NewString(),
		Config: &container.Config{
			Image:      image,
			Entrypoint: []string{"/usr/local/bin/wardyn-proxy"},
			Cmd:        []string{"-egress-canary", liveRelayProbeHost + ":" + strconv.Itoa(int(port))},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(netName),
			ExtraHosts:     []string{liveRelayProbeHost + ":" + target},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			Resources:      container.Resources{Memory: 128 << 20, MemorySwap: 128 << 20, PidsLimit: &pids},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("create: %w", err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}
	wait := cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.Error != nil {
			return 0, fmt.Errorf("wait: %s", res.Error.Message)
		}
		return res.StatusCode, nil
	case err := <-wait.Error:
		return 0, fmt.Errorf("wait: %w", err)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
