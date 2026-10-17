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
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

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
	sub := runnertest.NewFake()
	rt, err := localRunnerRuntime(ctx, runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}, stateDir, sub, ref, newLocalRelay(ctx), unsized)
	if err != nil {
		t.Fatalf("the re-registered runner did not start: %v", err)
	}
	t.Cleanup(func() { _ = rt.close() })
	if !sub.Called("stop_proxy", ref(run)) {
		t.Fatalf("the earlier registration's run proxy was not stopped: %v", sub.Calls)
	}
	if moved, _ := filepath.Glob(filepath.Join(stateDir, "previous", earlier.String()+"-*", "offline", "state.json")); len(moved) != 1 {
		t.Fatalf("the earlier registration's state was not set aside: %v", moved)
	}
}
