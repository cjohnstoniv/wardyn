// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func TestServeValidatesIdentityBeforeDependencies(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	if err := runneridentity.Save(stateDir, identity); err != nil {
		t.Fatal(err)
	}
	called := false
	cmd := newServeCommand(func(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
		called = true
		return runnerRuntime{}, nil
	})
	cmd.SetArgs([]string{"--org", "https://other.example", "--state-dir", stateDir})
	if err := cmd.Execute(); err == nil || called {
		t.Fatalf("wrong org reached dependencies: %v called=%v", err, called)
	}
}

func TestServeMissingProductionHooksRefuses(t *testing.T) {
	if _, err := productionRunnerHooks(context.Background(), runneridentity.Identity{}, t.TempDir(), serveOptions{}); err == nil {
		t.Fatal("missing durable handlers accepted")
	}
}

func TestServeRecordingModeMatchesCapture(t *testing.T) {
	for _, tc := range []struct {
		mode string
		on   bool
		bad  bool
	}{{"pg", true, false}, {"fs", true, false}, {"off", false, false}, {"unknown", false, true}} {
		got, err := (serveOptions{recording: tc.mode}).recordingEnabled()
		if got != tc.on || (err != nil) != tc.bad {
			t.Fatalf("mode=%s enabled=%v error=%v", tc.mode, got, err)
		}
	}
}
