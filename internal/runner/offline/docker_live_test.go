// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package offline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"
)

// The real proxy enforces cached policy and writes the private mounted spool;
// authenticated transport loss then exercises the real Docker containment.
// Only elapsed monotonic time is advanced, not a sleep standing in for lapse.
func TestLiveDockerOfflineLapsePreservesAgentFilesAndAudit(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set")
	}
	binary := os.Getenv("WARDYN_TEST_PROXY_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("WARDYN_TEST_PROXY_BINARY must name the current static proxy binary")
	}
	for _, realWire := range []bool{false, true} {
		name := "loopback"
		if realWire {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) { liveOfflineDocker(t, binary, realWire) })
	}
}
func liveOfflineDocker(t *testing.T, binary string, realWire bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	network, err := cli.NetworkCreate(ctx, "wardyn-offline-"+uuid.NewString(), dockerclient.NetworkCreateOptions{Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_, _ = cli.NetworkRemove(clean, network.ID, dockerclient.NetworkRemoveOptions{})
	}()
	dir := t.TempDir()
	sub, err := docker.New(docker.Config{ProxyImage: "busybox:latest", ProxyBinaryHostPath: binary, InternalNetwork: network.ID, RunnerStateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	f := connectWireManager(t, dir, sub, docker.SandboxRef, realWire)
	spec := runner.SandboxSpec{RunID: uuid.New(), Image: "busybox:latest", ConfinementClass: types.CC1, ProxyConfig: runner.ProxyConfig{RunToken: "offline-conformance-token", ControlPlaneURL: "http://127.0.0.1:9", Policy: types.RunPolicySpec{MinConfinementClass: types.CC1}}}
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
	code, out := liveExec(t, ctx, sub, sb.Ref, "printf kept >/tmp/wardyn-offline-kept; test ! -e /var/lib/wardyn/decision-spool")
	if code != 0 {
		t.Fatalf("agent mount separation: %d %s", code, out)
	}
	f.drop(t)
	code, out = liveExec(t, ctx, sub, sb.Ref, "http_proxy=http://wardyn-proxy:3128 wget -S -T 5 -O - http://denied.invalid/offline 2>&1")
	if code == 0 || !strings.Contains(out, "403") {
		t.Fatalf("offline proxy did not enforce cached deny: %d %s", code, out)
	}
	f.wallJump.Store(int64(-365 * 24 * time.Hour))
	f.elapsed.Store(int64(identity.RunTokenLapseAfter))
	if err := f.manager.CheckLapses(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := sub.Status(ctx, sb.Ref)
	if err != nil || st.State != types.RunRunning {
		t.Fatalf("lapse destroyed agent: %+v %v", st, err)
	}
	code, out = liveExec(t, ctx, sub, sb.Ref, "cat /tmp/wardyn-offline-kept")
	if code != 0 || out != "kept" {
		t.Fatalf("lapse lost writable files: %d %q", code, out)
	}
	proxyName := "wardyn-proxy-" + spec.RunID.String()
	if _, err := cli.ContainerInspect(ctx, proxyName, dockerclient.ContainerInspectOptions{}); err == nil {
		t.Fatal("expired proxy still exists")
	}
	spool, err := runnerspool.Open(filepath.Join(dir, "spools", spec.RunID.String()), spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	rows, err := spool.After(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Decision.Request.Host == "denied.invalid" && row.ValidHash() {
			found = true
		}
	}
	if !found {
		t.Fatalf("proxy decision did not survive containment: %+v", rows)
	}
	if err := f.manager.BeforeCreate(ctx, runner.SandboxSpec{RunID: uuid.New()}); err == nil {
		t.Fatal("offline create admitted")
	}
}
func liveExec(t *testing.T, ctx context.Context, sub *docker.Driver, ref, command string) (int, string) {
	t.Helper()
	session, err := sub.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh", "-c", command}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, session.Stderr); close(drained) }()
	out, err := io.ReadAll(session.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	code, err := session.Wait()
	if err != nil {
		t.Fatal(err)
	}
	<-drained
	return code, string(out)
}
