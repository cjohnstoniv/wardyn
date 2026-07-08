// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

// standalone_docker_test.go is the end-to-end integration test for the
// wardyn-runner STANDALONE mode (the `-tags docker` build whose real main()/run()
// live in main.go). It exercises the exact path the binary takes — load a JSON
// -spec, drive the docker driver through create -> exec -> wait -> status ->
// teardown — against a LIVE Docker daemon, and asserts capability honesty end to
// end. It mirrors the conformance recipe (busybox sandbox + busybox proxy on the
// pre-existing 'wardyn-internal' network).
//
// Guarded by WARDYN_TEST_DOCKER=1: it skips cleanly when unset so plain unit-CI
// (which runs the default-build, no-daemon main_test.go in this same package)
// never needs Docker. The //go:build docker tag matches main.go's, so this file
// compiles only under `-tags docker` alongside the real standalone runner.
//
// Symbol hygiene: main_test.go is UN-tagged and therefore also compiles under
// `-tags docker`, so every helper here is uniquely named (sd* prefix) to avoid
// colliding with that file's identifiers.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	dockerdriver "github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dockerclientFromEnv builds a daemon client the same way the driver does
// (FromEnv + API-version negotiation). Used by helpers that read daemon state
// the driver does not expose (network presence, raw exec output).
func dockerclientFromEnv() (*dockerclient.Client, error) {
	return dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
}

// sdSkipNoDocker centralizes the WARDYN_TEST_DOCKER skip-guard so every case here
// skips cleanly without the live substrate but RUNS (and must pass) with it.
func sdSkipNoDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the wardyn-runner standalone docker e2e")
	}
}

// sdEnsureInternalNetwork creates the control-plane-facing network the proxy
// sidecar joins at create time, best-effort (ignore "already exists"). Without
// it CreateSandbox fails immediately.
func sdEnsureInternalNetwork(t *testing.T, d *dockerdriver.Driver, name string) {
	t.Helper()
	cli, err := dockerclientFromEnv()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	_, err = cli.NetworkCreate(context.Background(), name, network.CreateOptions{Driver: "bridge"})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "exists") {
		t.Logf("ensure network %q: %v (continuing; may already exist)", name, err)
	}
}

// TestStandalone_SpecContract_FullMapping covers the -spec JSON contract through
// the REAL loadSpec from main.go (docker build): a fully-populated spec file
// deserializes field-for-field into the runner.SandboxSpec the binary feeds the
// driver. Unlike main_test.go (which re-declares the schema for the no-docker
// build), this exercises the production loadSpec directly.
func TestStandalone_SpecContract_FullMapping(t *testing.T) {
	id := uuid.New()
	path := sdWriteSpecFile(t, `{
		"run_id": "`+id.String()+`",
		"image": "busybox:latest",
		"confinement_class": "CC1",
		"env": {"FOO": "bar"},
		"resources": {"cpu_millis": 750, "memory_mib": 128, "disk_mib": 1024},
		"labels": {"team": "research"}
	}`)

	spec, err := loadSpec(path)
	if err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	if spec.RunID != id {
		t.Errorf("RunID = %s, want parsed %s", spec.RunID, id)
	}
	if spec.Image != "busybox:latest" {
		t.Errorf("Image = %q, want busybox:latest", spec.Image)
	}
	if spec.ConfinementClass != types.CC1 {
		t.Errorf("ConfinementClass = %q, want CC1", spec.ConfinementClass)
	}
	if spec.Env["FOO"] != "bar" {
		t.Errorf("Env = %v, want FOO=bar", spec.Env)
	}
	if spec.Resources.CPUMillis != 750 || spec.Resources.MemoryMiB != 128 || spec.Resources.DiskMiB != 1024 {
		t.Errorf("Resources = %+v, want {750 128 1024}", spec.Resources)
	}
	if spec.Labels["team"] != "research" {
		t.Errorf("Labels = %v, want team=research", spec.Labels)
	}
}

// TestStandalone_SpecContract_Defaults pins loadSpec's required/defaulting rules
// in the production code path: image is required, an omitted run_id is freshly
// generated, and a malformed run_id is a hard error (never silently replaced).
func TestStandalone_SpecContract_Defaults(t *testing.T) {
	// image required.
	if _, err := loadSpec(sdWriteSpecFile(t, `{"confinement_class":"CC1"}`)); err == nil {
		t.Error("missing image must error")
	}
	// omitted run_id -> fresh, non-nil, and different across two loads.
	a, err := loadSpec(sdWriteSpecFile(t, `{"image":"busybox:latest"}`))
	if err != nil {
		t.Fatalf("loadSpec a: %v", err)
	}
	b, err := loadSpec(sdWriteSpecFile(t, `{"image":"busybox:latest"}`))
	if err != nil {
		t.Fatalf("loadSpec b: %v", err)
	}
	if a.RunID == uuid.Nil {
		t.Error("omitted run_id must generate a non-nil UUID")
	}
	if a.RunID == b.RunID {
		t.Errorf("omitted run_id must be freshly generated; got identical %s", a.RunID)
	}
	// malformed run_id -> hard error.
	if _, err := loadSpec(sdWriteSpecFile(t, `{"image":"busybox:latest","run_id":"not-a-uuid"}`)); err == nil {
		t.Error("malformed run_id must error")
	}
}

// TestStandalone_CapabilityHonesty asserts the docker driver the binary builds
// reports ONLY what this host can actually enforce, end to end: Driver=="docker"
// matching Name(), StructuralEgress=true (L0 IS wired here), CC1 always present
// as the strongest-last list, and L1 NetworkPolicy honestly false (it is not
// implemented in v0). Overclaiming would let the control plane schedule an
// unenforced run (invariant 5). This is the -capabilities path of run().
func TestStandalone_CapabilityHonesty(t *testing.T) {
	sdSkipNoDocker(t)

	drv, err := dockerdriver.New(dockerdriver.Config{ProxyImage: "busybox:latest"})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	caps, err := orchestrator.New(drv).Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Driver != drv.Name() {
		t.Errorf("Capabilities.Driver = %q, want Name() = %q", caps.Driver, drv.Name())
	}
	if caps.Driver != "docker" {
		t.Errorf("Driver = %q, want docker", caps.Driver)
	}
	if !caps.StructuralEgress {
		t.Error("docker driver must declare StructuralEgress (L0 is structural here)")
	}
	if !caps.SessionRecording {
		t.Error("docker driver supports wardyn-rec, must declare SessionRecording")
	}
	if caps.NetworkPolicy {
		t.Error("L1 NetworkPolicy is not implemented in v0; declaring it would overclaim (invariant 5)")
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Fatal("docker driver must declare at least CC1")
	}
	if caps.ConfinementClasses[0] != types.CC1 {
		t.Errorf("ConfinementClasses must start with CC1 (always-available, strongest last), got %v", caps.ConfinementClasses)
	}
}

// TestStandalone_LifecycleEndToEnd is the headline e2e: it walks the standalone
// runner's full sequence against a live daemon — load a -spec, CreateSandbox,
// Exec a short-lived command, Wait for its exit code, read Status, then tear the
// sandbox down with KillSandbox and confirm the per-run network is gone. It
// mirrors run() in main.go and the conformance Wait/Status cases, but pins the
// concrete docker driver the binary actually uses.
func TestStandalone_LifecycleEndToEnd(t *testing.T) {
	sdSkipNoDocker(t)

	drv, err := dockerdriver.New(dockerdriver.Config{ProxyImage: "busybox:latest"})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	sdEnsureInternalNetwork(t, drv, "wardyn-internal")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Load the spec exactly as the binary does (production loadSpec), then
	// stamp the ProxyConfig the way run() does from -token/-control-plane.
	id := uuid.New()
	spec, err := loadSpec(sdWriteSpecFile(t, `{
		"run_id": "`+id.String()+`",
		"image": "busybox:latest",
		"confinement_class": "CC1",
		"resources": {"cpu_millis": 500, "memory_mib": 128}
	}`))
	if err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	spec.ProxyConfig = runner.ProxyConfig{RunToken: "standalone-tok", ControlPlaneURL: "http://127.0.0.1:0"}

	sb, err := drv.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	// Idempotent teardown safety net even if the explicit Kill below is skipped.
	t.Cleanup(func() { _ = drv.KillSandbox(context.Background(), sb.Ref) })

	if sb.Driver != "docker" {
		t.Errorf("Sandbox.Driver = %q, want docker", sb.Driver)
	}
	// Capability honesty at the create boundary: the enforced class must not be
	// silently downgraded below what was requested (CC1 here).
	if sb.EnforcedClass != types.CC1 {
		t.Errorf("EnforcedClass = %q, want CC1 (no silent downgrade)", sb.EnforcedClass)
	}

	// Status right after create reports RUNNING (the agent holds open on
	// `sleep infinity`).
	st, err := drv.Status(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != types.RunRunning {
		t.Errorf("post-create State = %q, want RUNNING", st.State)
	}

	// Exec a short-lived command with a distinctive non-zero exit code and Wait
	// for it: this is the COMPLETED-vs-FAILED signal the control plane maps from
	// the agent process exit code. A distinctive 42 cannot pass by an accidental
	// 0/1.
	const wantCode = 42
	if err := drv.Exec(ctx, sb.Ref, []string{"sh", "-c", "exit 42"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got, err := drv.Wait(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got != wantCode {
		t.Errorf("Wait exit code = %d, want %d", got, wantCode)
	}

	// Teardown: KillSandbox removes the agent + proxy + per-run internal network.
	if err := drv.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	// Per-run internal network must be gone (teardown cascaded).
	if sdNetworkExists(t, "wardyn-int-"+id.String()) {
		t.Errorf("per-run internal network wardyn-int-%s must be removed on teardown", id.String())
	}
	// Status after kill reports STOPPED (not an error) — the container is gone.
	st, err = drv.Status(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Status after kill: %v", err)
	}
	if st.State != types.RunStopped {
		t.Errorf("post-kill State = %q, want STOPPED", st.State)
	}
	// Second KillSandbox must be idempotent (the kill-switch contract).
	if err := drv.KillSandbox(ctx, sb.Ref); err != nil {
		t.Errorf("second KillSandbox must be idempotent, got %v", err)
	}
}

// TestStandalone_L0AfterStandaloneCreate confirms the L0 structural-egress
// invariant holds for a sandbox created via the standalone path: the agent has
// NO default route. This is the binary-level analogue of the docker package's
// network_test.go L0 probe — it guards against a standalone-specific
// regression (e.g. a different Config that accidentally grants a route).
func TestStandalone_L0AfterStandaloneCreate(t *testing.T) {
	sdSkipNoDocker(t)

	drv, err := dockerdriver.New(dockerdriver.Config{ProxyImage: "busybox:latest"})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	sdEnsureInternalNetwork(t, drv, "wardyn-internal")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	spec, err := loadSpec(sdWriteSpecFile(t, `{"image":"busybox:latest","confinement_class":"CC1"}`))
	if err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	spec.ProxyConfig = runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: "http://127.0.0.1:0"}

	sb, err := drv.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = drv.KillSandbox(context.Background(), sb.Ref) })

	out := sdExecCapture(ctx, t, sb.Ref, []string{"ip", "route"})
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "default") {
			t.Errorf("L0 violated: standalone sandbox %q has a default route.\nip route:\n%s", sb.Ref, out)
		}
	}
	if !strings.Contains(out, "dev eth0") {
		t.Errorf("standalone sandbox must have an on-link route on the per-run net, got:\n%s", out)
	}
}

// --- helpers (sd* prefix to avoid colliding with main_test.go) ---------------

// sdWriteSpecFile writes a JSON spec to a temp file and returns its path, so the
// production loadSpec (which reads from disk) is exercised exactly as the binary
// uses it.
func sdWriteSpecFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write spec file: %v", err)
	}
	return path
}

// sdExecCapture execs argv in ref and returns the demultiplexed combined output.
// Non-TTY exec uses Docker's framed stream protocol, demuxed with stdcopy.
func sdExecCapture(ctx context.Context, t *testing.T, ref string, argv []string) string {
	t.Helper()
	cli, err := dockerclientFromEnv()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	created, err := cli.ContainerExecCreate(ctx, ref, container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          argv,
	})
	if err != nil {
		t.Fatalf("exec create %v: %v", argv, err)
	}
	resp, err := cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach %v: %v", argv, err)
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, resp.Reader); err != nil && err != io.EOF {
		t.Logf("exec %v: stdcopy warning: %v (continuing)", argv, err)
	}
	return stdout.String() + stderr.String()
}

// sdNetworkExists reports whether a docker network by name is present.
func sdNetworkExists(t *testing.T, name string) bool {
	t.Helper()
	cli, err := dockerclientFromEnv()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	_, err = cli.NetworkInspect(context.Background(), name, network.InspectOptions{})
	return err == nil
}
