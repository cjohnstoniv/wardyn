// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The composed runtime's caps event names every run proxy setting this build carries: the org
// refuses a run that sets one a runner does not name, so a runner that dropped them would refuse
// those runs on every version.
func TestRunnerRuntimeAdvertisesEveryProxySetting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}
	runtime, err := runnerRuntimeWith(ctx, identity, t.TempDir(), runnertest.NewFake(), func(id uuid.UUID) string { return "fake-" + id.String() },
		newLocalRelay(ctx), unsized, runtimeExtras{noPosture: true, arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	const orgURL = "https://org.example"
	row := types.Runner{ID: identity.RunnerID, PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256(orgURL)}
	link := &remote.Link{}
	remoteSub := remote.New(row.ID.String(), link, remote.Options{})
	hub := remote.NewHub(ctx, remote.HubConfig{OrgURL: orgURL,
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (remote.Admission, error) {
			return remote.Admission{Substrate: remoteSub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: runtime.hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, local := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return local, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = runRunnerServices(ctx, client.Run, runtime.run) }()
	defer func() { cancel(); <-done }()
	for !link.Online() {
		select {
		case <-done:
			t.Fatal("composed runtime stopped before READY")
		case <-ctx.Done():
			t.Fatal("composed runtime did not reach READY", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	caps, ok := remoteSub.Caps()
	if !ok || !slices.Equal(caps.ProxySettings, runnerwire.ProxySettings) {
		t.Fatalf("caps (ok=%v) advertise proxy settings %v, want %v", ok, caps.ProxySettings, runnerwire.ProxySettings)
	}
}
