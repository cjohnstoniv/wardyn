// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// TestReRegisteredRunnerStartsWithItsEarlierRunStateSetAside starts a runner registered again on a
// computer whose state directory still holds the earlier registration's run: it stops that run's
// proxy, sets the state aside and starts, instead of refusing the state on every start.
func TestReRegisteredRunnerStartsWithItsEarlierRunStateSetAside(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stateDir, earlier, run := t.TempDir(), uuid.New(), uuid.New()
	ref := func(id uuid.UUID) string { return "fake-" + id.String() }
	if err := os.MkdirAll(filepath.Join(stateDir, "offline"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := fmt.Sprintf(`{"runner_id":%q,"revoked":false,"runs":{%q:{"run_id":%q,"ref":%q,"lease":{"run_id":%q}}},"actions":{}}`, earlier, run, run, ref(run), run)
	if err := os.WriteFile(filepath.Join(stateDir, "offline", "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub := earlierSandboxes{Fake: runnertest.NewFake(), runnerID: earlier.String()}
	rt, err := localRunnerRuntime(ctx, runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}, stateDir, sub, ref, newLocalRelay(ctx), unsized, runtimeExtras{})
	if err != nil {
		t.Fatalf("the re-registered runner did not start: %v", err)
	}
	t.Cleanup(func() { _ = rt.close() })
	if !sub.Called("stop_proxy", ref(run)) {
		t.Fatalf("the earlier registration's run proxy was not stopped: %v", sub.Calls)
	}
	if moved, _ := filepath.Glob(filepath.Join(stateDir, "previous", earlier.String(), "offline", "state.json")); len(moved) != 1 {
		t.Fatalf("the earlier registration's state was not set aside: %v", moved)
	}
}

// TestReRegisteredDockerRunnerSetsAsideItsEarlierRunWithTheSink_Live is the same start as serve wires it
// for Docker: the real driver, its decision sink and the evidence forwarder before the manager exists,
// against a proxy container the earlier registration labelled. The proxy is removed, its settlement
// asks the forwarder for no evidence, and the runner starts with the state set aside.
// Skipped unless WARDYN_TEST_DOCKER=1.
func TestReRegisteredDockerRunnerSetsAsideItsEarlierRunWithTheSink_Live(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker re-registration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	stateDir, earlier, run := t.TempDir(), uuid.New(), uuid.New()
	proxy := "wardyn-proxy-" + run.String()
	sinkDocker(t, "run", "-d", "--name", proxy, "--label", "wardyn.managed=true", "--label", "wardyn.run-id="+run.String(),
		"--label", runner.LabelRunnerID+"="+earlier.String(), "busybox:latest", "sh", "-c", "trap 'exit 0' TERM; sleep 300 & wait")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", proxy).Run() })
	if err := os.MkdirAll(filepath.Join(stateDir, "offline"), 0o700); err != nil {
		t.Fatal(err)
	}
	ref := docker.SandboxRef(run)
	state := fmt.Sprintf(`{"runner_id":%q,"revoked":false,"runs":{%q:{"run_id":%q,"ref":%q,"lease":{"run_id":%q}}},"actions":{}}`, earlier, run, run, ref, run)
	if err := os.WriteFile(filepath.Join(stateDir, "offline", "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}
	sink, err := newDockerSink(ctx, stateDir, dockerEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ev := &evidenceForwarder{}
	sub, err := docker.New(docker.Config{RunnerID: identity.RunnerID.String(), ProxyImage: "busybox:latest",
		DecisionSink: sinkAdapter{s: sink.spoolSink, url: sink.proxyURL}, Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := localRunnerRuntime(ctx, identity, stateDir, sub, docker.SandboxRef, newLocalRelay(ctx), unsized, sink.extras(sub, ev))
	if err != nil {
		t.Fatalf("the re-registered runner did not start: %v", err)
	}
	t.Cleanup(func() { _ = rt.close() })
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name=^"+proxy+"$").Output(); len(out) != 0 {
		t.Fatalf("the earlier registration's proxy %s is still on the engine", proxy)
	}
	if moved, _ := filepath.Glob(filepath.Join(stateDir, "previous", earlier.String(), "offline", "state.json")); len(moved) != 1 {
		t.Fatalf("the earlier registration's state was not set aside: %v", moved)
	}
}

// earlierSandboxes proves a ref a sandbox only of runnerID, as the engine does for the containers
// that runner labelled.
type earlierSandboxes struct {
	*runnertest.Fake
	runnerID string
}

func (s earlierSandboxes) ProveRunnerSandbox(_ context.Context, ref, runnerID string) error {
	if runnerID != s.runnerID {
		return fmt.Errorf("%s is not runner %s's: %w", ref, runnerID, runner.ErrNotRunnerSandbox)
	}
	return nil
}
