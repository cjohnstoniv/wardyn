// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

func TestDispatcherWorkloadStatus(t *testing.T) {
	status := runnerwire.WorkloadStatusArgs{Namespace: "team-a", Name: "triage-bot", ObservedGeneration: 3, Phase: runnerwire.WorkloadPhaseRunning, RunID: uuid.NewString()}
	a, b := runnerwire.Loopback()
	defer b.Close()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer p.Close()
	call := func(d *Dispatcher, args any) error {
		data, _ := runnerwire.Marshal(args)
		_, err := d.Call(context.Background(), p, 2, runnerwire.MethodWorkloadStatus, data)
		return err
	}

	var got []runnerwire.WorkloadStatusArgs
	lifecycle := func(cfg Config) Config {
		cfg.BeforeCreate = func(context.Context, runner.SandboxSpec) error { return nil }
		cfg.Created = func(context.Context, runner.SandboxSpec, string) error { return nil }
		return cfg
	}
	failure := errors.New("patch failed")
	d, err := newTestDispatcher(lifecycle(Config{RunnerID: uuid.NewString(), Substrate: runnertest.NewFake(), WorkloadStatus: func(_ context.Context, a runnerwire.WorkloadStatusArgs) error {
		got = append(got, a)
		if a.Name == "fails" {
			return failure
		}
		return nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := call(d, status); err != nil || len(got) != 1 || got[0] != status {
		t.Fatalf("a valid status: err %v, handed over %+v", err, got)
	}
	bad := status
	bad.Phase = "Sleeping"
	if err := call(d, bad); !errors.Is(err, runnerwire.ErrBadFrame) || len(got) != 1 {
		t.Fatalf("an unknown phase: err %v, handed over %d, want refused before the adapter", err, len(got))
	}
	status.Name = "fails"
	if err := call(d, status); !errors.Is(err, failure) {
		t.Fatalf("the adapter's failure: %v, want it returned", err)
	}

	// A runner that lists no workload namespace refuses the call.
	plain, err := newTestDispatcher(lifecycle(Config{RunnerID: uuid.NewString(), Substrate: runnertest.NewFake()}))
	if err != nil {
		t.Fatal(err)
	}
	if err := call(plain, status); err == nil {
		t.Fatal("workload_status accepted by a runner with no workload namespaces")
	}
}
