// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

func TestDispatcherChecksRunnerAndDurableGuardBeforeCreate(t *testing.T) {
	id := uuid.NewString()
	fake := runnertest.NewFake()
	guard := errors.New("offline lease unavailable")
	d, err := New(Config{RunnerID: id, Substrate: fake, BeforeCreate: func(context.Context, runner.SandboxSpec) error { return guard }})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer p.Close()
	for _, runnerID := range []string{id, uuid.NewString()} {
		data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: uuid.New(), RunnerID: runnerID}})
		if _, err := d.Call(context.Background(), p, 2, runnerwire.MethodCreateSandbox, data); err == nil {
			t.Fatal("unsafe create accepted")
		}
	}
	if len(fake.Created) != 0 {
		t.Fatal("created before identity/offline checks")
	}
	data, _ := runnerwire.Marshal(runnerwire.RefArgs{Ref: placement.RunnerRefPrefix(uuid.NewString()) + "sandbox"})
	if _, err := d.Call(context.Background(), p, 2, runnerwire.MethodKill, data); err == nil {
		t.Fatal("foreign runner ref accepted")
	}
	if len(fake.Calls) != 0 {
		t.Fatal("foreign ref reached substrate")
	}
}

func TestDispatcherOutputBindingFailureRollsBackSandbox(t *testing.T) {
	fake := runnertest.NewFake()
	id, run := uuid.NewString(), uuid.New()
	failure := errors.New("persist output binding failed")
	d, err := New(Config{RunnerID: id, Substrate: fake, BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Output: func(uuid.UUID) (io.Writer, error) { return io.Discard, nil }, BindOutput: func(uuid.UUID, string) error { return failure },
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer p.Close()
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: run, RunnerID: id}, ExecOutput: true})
	if _, err := d.Call(context.Background(), p, 2, runnerwire.MethodCreateSandbox, data); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !fake.Called("kill", "fake-"+run.String()) {
		t.Fatal("unreported sandbox remained running")
	}
}
