// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

// conformance_docker_test.go runs the conformance suite against the docker
// driver. The test is guarded by WARDYN_TEST_DOCKER=1; it is skipped cleanly
// when that variable is unset, so it never requires Docker in unit-CI.
//
// The //go:build docker tag ensures this file is only compiled when the caller
// passes -tags docker (matching the docker driver's own build tag). CI sets
// WARDYN_TEST_DOCKER=1 and builds with -tags docker to run real conformance.

import (
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

func TestConformanceDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping docker conformance")
	}

	sub, err := docker.New(docker.Config{
		// busybox doubles as the proxy image so no real wardyn-proxy binary is
		// required for the conformance gate. busybox's default `sh` would exit
		// immediately (leaving the sidecar with no per-run network IP, failing
		// CreateSandbox), so keep it alive — the sidecar only needs to exist on
		// the network for the runner-contract assertions, not to relay traffic.
		ProxyImage: "busybox:latest",
		ProxyCmd:   []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	// Exercise the assembled production path: the orchestrator over the OCI
	// substrate is what the control plane actually runs.
	r := orchestrator.New(sub)
	requireStrongestClass(t, r)

	// The docker driver requires the control-plane-facing network to exist so
	// the proxy sidecar can join it at sandbox creation. Create best-effort;
	// ignore "already exists".
	ensureConformanceNetwork(t, "wardyn-internal")

	conformance.Run(t, r, conformance.Options{
		// busybox is our minimal image: it has ip(8) for the L0 probe and is
		// fast to pull.
		SandboxImage:      "busybox:latest",
		DefaultRouteProbe: dockerRouteProbe,
		Timeout:           3 * time.Minute,
		// busybox's sh can exit with an explicit code; the Wait conformance case
		// Execs this and asserts Wait returns the same code.
		ExitArgv: func(code int) []string {
			return []string{"sh", "-c", "exit " + strconv.Itoa(code)}
		},
		// busybox runs as root, and a root probe can chmod or rename a
		// root-owned file without any capability; the managed-files case runs
		// as the uid every agent image uses instead.
		AgentUserImage: agentUserImage(t, "busybox:latest"),
	})
}

// requireStrongestClass fails the leg when WARDYN_TEST_REQUIRE_CLASS is set and
// is not the strongest class the daemon advertises. Every conformance case runs
// at the strongest class, so this is what makes the nightly gVisor leg run
// ManagedFiles, L0StructuralEgress and ExecStream under runsc — and go red, not
// quietly back to runc, on a runner where runsc did not register.
func requireStrongestClass(t *testing.T, r runner.Runner) {
	t.Helper()
	want := types.ConfinementClass(os.Getenv("WARDYN_TEST_REQUIRE_CLASS"))
	if want == "" {
		return
	}
	caps, err := r.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if n := len(caps.ConfinementClasses); n == 0 || caps.ConfinementClasses[n-1] != want {
		t.Fatalf("WARDYN_TEST_REQUIRE_CLASS=%s but the strongest class this daemon advertises is not it (classes %v): is its runtime installed and registered with dockerd?",
			want, caps.ConfinementClasses)
	}
}

// agentUserImage commits base with USER 1000:1000 — the image contract's
// agent identity — under a unique tag removed at cleanup.
func agentUserImage(t *testing.T, base string) string {
	t.Helper()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatalf("agentUserImage: create client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := cli.ImageInspect(ctx, base); err != nil {
		if err := dockerutil.PullImage(ctx, cli, base, "agentUserImage"); err != nil {
			t.Fatal(err)
		}
	}
	suffix := uuid.NewString()[:8]
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name:   "wardyn-conformance-uid1000-" + suffix,
		Config: &container.Config{Image: base, Cmd: []string{"true"}},
	})
	if err != nil {
		t.Fatalf("agentUserImage: create: %v", err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	}()
	ref := "wardyn-conformance-uid1000:" + suffix
	if _, err := cli.ContainerCommit(ctx, created.ID, dockerclient.ContainerCommitOptions{Reference: ref, Changes: []string{"USER 1000:1000"}}); err != nil {
		t.Fatalf("agentUserImage: commit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ImageRemove(context.Background(), ref, dockerclient.ImageRemoveOptions{Force: true, PruneChildren: true})
	})
	return ref
}

// dockerRouteProbe is the L0 DefaultRouteProbe for the docker driver.
//
// It execs `ip route` inside the agent container identified by ref and calls
// t.Errorf if any line in the output starts with the word "default", which
// would indicate a default route (0.0.0.0/0) is reachable from the sandbox
// (L0 structural-egress violation).
//
// Implementation notes:
//   - Non-TTY exec attach uses Docker's multiplexed stream protocol; output
//     is demultiplexed with stdcopy.StdCopy so binary frame headers do not
//     corrupt the text comparison.
//   - busybox's `ip route show default` prints ALL routes (unlike iproute2
//     proper), so we exec `ip route` and check for the "default" keyword
//     ourselves — the same approach used in integration_test.go.
func dockerRouteProbe(t *testing.T, ref string) {
	t.Helper()

	cli, err := dockerclient.New(
		dockerclient.FromEnv,
	)
	if err != nil {
		t.Fatalf("dockerRouteProbe: create client: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// `ip route` lists the full routing table. On a container attached only to
	// an Internal=true network (no gateway), there is no "default" route entry.
	created, err := cli.ExecCreate(ctx, ref, dockerclient.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"ip", "route"},
	})
	if err != nil {
		t.Fatalf("dockerRouteProbe: exec create: %v", err)
	}

	attachRes, err := cli.ExecAttach(ctx, created.ID, dockerclient.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("dockerRouteProbe: exec attach: %v", err)
	}
	resp := attachRes.HijackedResponse
	defer resp.Close()

	// Demultiplex the Docker stream protocol (8-byte framed header per chunk)
	// into separate stdout/stderr buffers. Using io.Copy directly on resp.Reader
	// produces garbled output because the binary frame headers are included.
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, resp.Reader); err != nil && err != io.EOF {
		t.Logf("dockerRouteProbe: stdcopy warning: %v (continuing)", err)
	}

	output := stdout.String() + stderr.String()
	if strings.Contains(output, "default") {
		t.Errorf("L0 structural-egress violated: agent container %q has a default route.\nip route output:\n%s", ref, output)
	}
}

// ensureConformanceNetwork creates the named bridge network if it does not
// already exist. The docker driver's CreateSandbox connects the proxy sidecar
// to this network; without it sandbox creation fails immediately.
func ensureConformanceNetwork(t *testing.T, name string) {
	t.Helper()

	cli, err := dockerclient.New(
		dockerclient.FromEnv,
	)
	if err != nil {
		t.Fatalf("ensureConformanceNetwork: create client: %v", err)
	}
	defer cli.Close()

	ctx := context.Background()
	_, err = cli.NetworkCreate(ctx, name, dockerclient.NetworkCreateOptions{Driver: "bridge"})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "exists") {
		// Non-fatal: log and continue; the test will fail later if the
		// network is truly absent and the driver cannot connect the proxy.
		t.Logf("ensureConformanceNetwork %q: %v (may already exist)", name, err)
	}
}
