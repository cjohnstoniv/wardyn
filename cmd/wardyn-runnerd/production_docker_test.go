// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestLocalRuntimeRequiresLeaseAndPersistsCreation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}
	stateDir := t.TempDir()
	sub := runnertest.NewFake()
	ref := func(id uuid.UUID) string { return "fake-" + id.String() }
	runtime, err := localRunnerRuntime(ctx, identity, stateDir, sub, ref)
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
	var serviceErr error
	go func() { defer close(done); serviceErr = runRunnerServices(ctx, client.Run, runtime.run) }()
	defer func() { cancel(); <-done }()
	for !link.Online() {
		select {
		case <-done:
			t.Fatal("composed runtime stopped before READY", serviceErr)
		case <-ctx.Done():
			t.Fatal("composed runtime did not reach READY", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	run := uuid.New()
	spec := runner.SandboxSpec{RunID: run, RunnerID: row.ID.String(), ConfinementClass: types.CC1}
	if _, err := remoteSub.CreateSandbox(ctx, spec); err == nil {
		t.Fatal("production lifecycle hooks permitted create without an initial lease")
	}
	if _, err := sub.Status(ctx, ref(run)); !errors.Is(err, runner.ErrSandboxGone) {
		t.Fatalf("missing-lease create reached substrate: %v", err)
	}
	lease, _ := runnerwire.Marshal(runnerwire.Lease{RunID: run, TokenRenewedAt: time.Now(), EndsAt: time.Now().Add(time.Hour)})
	if err := link.Send(ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteSub.CreateSandbox(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteSub.CreateSandbox(ctx, spec); err == nil {
		t.Fatal("production lifecycle hooks permitted ambiguous create retry")
	}
	cancel()
	// Both services stop before the lifecycle lock is released for restart.
	<-done
	if err := runtime.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := localRunnerRuntime(context.Background(), identity, stateDir, sub, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if !sub.Called("stop_proxy", ref(run)) {
		t.Fatal("daemon restart did not contain the durably bound proxy before connecting")
	}
	// The inventory is emitted only while execution admission is closed.
	var state []runnerwire.RunState
	err = restarted.hooks.StatePages(context.Background(), func(page runnerwire.StatePage) error {
		state = append(state, page.Runs...)
		return nil
	})
	if err != nil || len(state) != 1 || state[0].RunID != run || !state[0].SandboxPresent {
		t.Fatalf("actual durable lifecycle state missing: %#v %v", state, err)
	}
}
