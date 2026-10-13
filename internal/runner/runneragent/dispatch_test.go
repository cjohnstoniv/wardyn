// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

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
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake, BeforeCreate: func(context.Context, runner.SandboxSpec) error { return guard },
		Created: func(context.Context, runner.SandboxSpec, string) error {
			t.Fatal("created after failed guard")
			return nil
		},
	})
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
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake, BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created: func(context.Context, runner.SandboxSpec, string) error { return nil },
		Output:  func(uuid.UUID) (io.Writer, error) { return io.Discard, nil }, BindOutput: func(uuid.UUID, string) error { return failure },
		ForgetOutput: func(uuid.UUID) error { return nil },
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

type cancelAfterCreate struct {
	*runnertest.Fake
	cancel context.CancelFunc
}

func (f cancelAfterCreate) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	sandbox, err := f.Fake.CreateSandbox(ctx, spec)
	f.cancel()
	return sandbox, err
}

func TestDispatcherDurableBindingSurvivesCallCancellation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "persist", true: "rollback"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := runnertest.NewFake()
			id, run := uuid.NewString(), uuid.New()
			failure := errors.New("local state could not be persisted")
			bound := false
			d, err := newTestDispatcher(Config{RunnerID: id, Substrate: cancelAfterCreate{fake, cancel},
				BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
				Created: func(persist context.Context, spec runner.SandboxSpec, ref string) error {
					if persist.Err() != nil || ctx.Err() == nil || spec.RunID != run || ref != "fake-"+run.String() {
						t.Fatal("lost local identity or inherited dropped call cancellation")
					}
					if _, bounded := persist.Deadline(); !bounded {
						t.Fatal("durable binding is unbounded")
					}
					bound = true
					if fail {
						return failure
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			a, b := runnerwire.Loopback()
			defer b.Close()
			peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
			defer peer.Close()
			data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: run, RunnerID: id}})
			_, err = d.Call(ctx, peer, 2, runnerwire.MethodCreateSandbox, data)
			if !bound || errors.Is(err, failure) != fail || (err != nil && !fail) {
				t.Fatalf("binding result: bound=%v error=%v", bound, err)
			}
			if fake.Called("kill", "fake-"+run.String()) != fail {
				t.Fatal("sandbox cleanup did not distinguish durable failure from link cancellation")
			}
		})
	}
}

// These tests isolate creation; lifecycle policy is exercised separately.
func newTestDispatcher(cfg Config) (*Dispatcher, error) {
	if cfg.CreateFailed == nil {
		cfg.CreateFailed = func(context.Context, runner.SandboxSpec) error { return nil }
	}
	if cfg.HoldsSandbox == nil {
		cfg.HoldsSandbox = func(string) bool { return true }
	}
	cfg.BeforeRefCall = func(context.Context, string, string, uuid.UUID) error { return nil }
	cfg.AfterRefCall = func(context.Context, string, string, string, uuid.UUID) error { return nil }
	if cfg.ExecutionResult == nil {
		cfg.ExecutionResult = func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error) {
			return runnerwire.ExecutionResult{}, runnerwire.Refuse("execution recovery not configured in this fixture")
		}
	}
	if cfg.MutationFinished == nil {
		cfg.MutationFinished = func(context.Context, string, string, uuid.UUID, uuid.UUID, error) error { return nil }
	}
	return New(cfg)
}

// sweepWithSeedError lists one orphan candidate and answers with an error, as a
// docker substrate does when a leftover drive seed would not be removed.
type sweepWithSeedError struct {
	*runnertest.Fake
	candidate uuid.UUID
}

func (s sweepWithSeedError) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	isOrphan(s.candidate)
	return 0, errors.New("docker: remove leftover drive seed: in use")
}

// TestDispatcherSweepListsCandidatesDespiteASeedError: the listing call still
// hands the org its candidates when the same pass failed to remove a drive
// seed, so one stuck seed does not stop every orphan sandbox's removal.
func TestDispatcherSweepListsCandidatesDespiteASeedError(t *testing.T) {
	orphan := uuid.New()
	d, err := newTestDispatcher(Config{RunnerID: uuid.NewString(), Substrate: sweepWithSeedError{Fake: runnertest.NewFake(), candidate: orphan},
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := runnerwire.Marshal(runnerwire.SweepArgs{MinAge: time.Minute})
	res, err := d.sweep(context.Background(), data)
	if err != nil {
		t.Fatalf("listing sweep = %v, want the candidates", err)
	}
	if got := res.(runnerwire.SweepResult).Candidates; len(got) != 1 || got[0] != orphan {
		t.Fatalf("candidates = %v, want [%s]", got, orphan)
	}
}
