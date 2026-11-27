// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package offline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With the organisation unreachable, a contain request stops the run's real proxy container on
// the next tick, leaves the agent running, and answers stopped.
func TestContainLive_DockerProxyStopped(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set")
	}
	binary := filepath.Join(t.TempDir(), "wardyn-proxy")
	build := exec.Command("go", "build", "-o", binary, "../../../cmd/wardyn-proxy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wardyn-proxy: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	network, err := cli.NetworkCreate(ctx, "wardyn-contain-"+uuid.NewString(), dockerclient.NetworkCreateOptions{Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_, _ = cli.NetworkRemove(clean, network.ID, dockerclient.NetworkRemoveOptions{})
	}()
	sub, err := docker.New(docker.Config{ProxyImage: "busybox:latest", ProxyBinaryHostPath: binary, InternalNetwork: network.ID})
	if err != nil {
		t.Fatal(err)
	}
	f := connectWireManager(t, t.TempDir(), sub, docker.SandboxRef, false)
	spec := runner.SandboxSpec{RunID: uuid.New(), Image: "busybox:latest", ConfinementClass: types.CC1, ProxyConfig: runner.ProxyConfig{RunToken: "contain-live-token", ControlPlaneURL: "http://127.0.0.1:9", Policy: types.RunPolicySpec{MinConfinementClass: types.CC1}}}
	f.initialLease(t, spec.RunID)
	if err := f.manager.BeforeCreate(ctx, spec); err != nil {
		t.Fatal(err)
	}
	sb, err := sub.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := sub.KillSandbox(clean, sb.Ref); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	if err := f.manager.Created(ctx, spec, sb.Ref); err != nil {
		t.Fatal(err)
	}
	proxyName := "wardyn-proxy-" + spec.RunID.String()
	if _, err := cli.ContainerInspect(ctx, proxyName, dockerclient.ContainerInspectOptions{}); err != nil {
		t.Fatalf("the proxy is not up before the contain: %v", err)
	}
	f.drop(t)
	rid := putContainRequest(t, f.manager, containRequest{Runs: []uuid.UUID{spec.RunID}, RequestedAt: time.Now().UTC(), UID: os.Getuid()})
	if err := containTick(t, f.manager); err != nil {
		t.Fatal(err)
	}
	if rc := readReceipt(t, f.manager, rid); rc.Runs[spec.RunID] != containStopped {
		t.Fatalf("receipt = %+v, want stopped", rc)
	}
	if _, err := cli.ContainerInspect(ctx, proxyName, dockerclient.ContainerInspectOptions{}); err == nil {
		t.Fatal("the contained run's proxy still exists")
	}
	if st, err := sub.Status(ctx, sb.Ref); err != nil || st.State != types.RunRunning {
		t.Fatalf("the contain destroyed the agent: %+v %v", st, err)
	}
}
