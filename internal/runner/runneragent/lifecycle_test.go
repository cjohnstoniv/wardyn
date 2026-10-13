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

type failedCreate struct{ *runnertest.Fake }

func (f failedCreate) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	_, _ = f.Fake.CreateSandbox(ctx, spec)
	return runner.Sandbox{}, errors.New("create response lost and cleanup unconfirmed")
}

func TestDispatcherRetainsOutputForAmbiguousCreateAndRefusesRetry(t *testing.T) {
	for _, existing := range []bool{false, true} {
		id, run := uuid.NewString(), uuid.New()
		intent, outputs, forgotten, contained := existing, 0, 0, 0
		d, err := newTestDispatcher(Config{RunnerID: id, Substrate: failedCreate{runnertest.NewFake()},
			BeforeCreate: func(context.Context, runner.SandboxSpec) error {
				if intent {
					return errors.New("existing or ambiguous creation intent")
				}
				intent = true
				return nil
			},
			Created: func(context.Context, runner.SandboxSpec, string) error {
				t.Fatal("created despite provisioning failure")
				return nil
			},
			CreateFailed: func(_ context.Context, spec runner.SandboxSpec) error {
				if spec.RunID != run {
					t.Fatal("contained another run")
				}
				contained++
				return nil
			},
			Output:     func(uuid.UUID) (io.Writer, error) { outputs++; return io.Discard, nil },
			BindOutput: func(uuid.UUID, string) error { t.Fatal("bound despite provisioning failure"); return nil },
			ForgetOutput: func(got uuid.UUID) error {
				if got != run {
					t.Fatal("forgot another run")
				}
				forgotten++
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		a, b := runnerwire.Loopback()
		peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
		defer peer.Close()
		defer b.Close()
		data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: run, RunnerID: id}, ExecOutput: true})
		for range 2 {
			if _, err := d.Call(context.Background(), peer, 2, runnerwire.MethodCreateSandbox, data); err == nil {
				t.Fatal("failed create or ambiguous retry reported success")
			}
		}
		want := 1
		if existing {
			want = 0
		}
		if outputs != want || forgotten != 0 || contained != want {
			t.Fatalf("existing=%v outputs=%d forgotten=%d contained=%d", existing, outputs, forgotten, contained)
		}
	}
}

type cancelAfterExec struct {
	*runnertest.Fake
	cancel context.CancelFunc
}

func (f cancelAfterExec) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	id, err := f.Fake.Exec(ctx, ref, argv)
	f.cancel()
	return id, err
}

func TestDispatcherRefGuardAndDurableExecIdentity(t *testing.T) {
	id, run := uuid.NewString(), uuid.New()
	fake := runnertest.NewFake()
	sb, err := fake.CreateSandbox(context.Background(), runner.SandboxSpec{RunID: run})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allowed, bound := false, false
	generation := uuid.New()
	d, err := New(Config{RunnerID: id, Substrate: cancelAfterExec{fake, cancel},
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
		CreateFailed: func(context.Context, runner.SandboxSpec) error { return nil },
		ExecutionResult: func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error) {
			return runnerwire.ExecutionResult{}, runnerwire.Refuse("not queried in this fixture")
		},
		BeforeRefCall: func(_ context.Context, method, ref string, gotGeneration uuid.UUID) error {
			if method != runnerwire.MethodExec || ref != sb.Ref || gotGeneration != generation {
				t.Fatal("wrong mutation guard identity")
			}
			if !allowed {
				return errors.New("no live lease")
			}
			return nil
		},
		MutationFinished: func(context.Context, string, string, uuid.UUID, uuid.UUID, error) error { return nil },
		AfterRefCall: func(persist context.Context, method, ref, execID string, gotGeneration uuid.UUID) error {
			if persist.Err() != nil || ctx.Err() == nil || method != runnerwire.MethodExec || ref != sb.Ref || execID == "" || gotGeneration != generation {
				t.Fatal("lost successful exec identity after wire cancellation")
			}
			if _, ok := persist.Deadline(); !ok {
				t.Fatal("unbounded persistence")
			}
			bound = true
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
	data, _ := runnerwire.Marshal(runnerwire.ExecArgs{ExecutionGeneration: generation, Ref: placement.RunnerRefPrefix(id) + sb.Ref, Argv: []string{"agent"}})
	if _, err := d.Call(ctx, peer, 2, runnerwire.MethodExec, data); err == nil || fake.Called("exec", sb.Ref) || bound {
		t.Fatal("agent started before lease guard")
	}
	allowed = true
	if _, err := d.Call(ctx, peer, 2, runnerwire.MethodExec, data); err != nil || !bound {
		t.Fatalf("agent exec was not durably recorded: %v", err)
	}
}
