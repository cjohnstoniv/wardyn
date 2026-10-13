// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/google/uuid"
)

type failedMutation struct {
	*runnertest.Fake
	cause  error
	cancel context.CancelFunc
}

func (s failedMutation) Exec(context.Context, string, []string) (string, error) {
	s.cancel()
	return "ambiguous-handle", s.cause
}

func TestMutationFinishedRequiredAndRunsAfterSubstrateFailure(t *testing.T) {
	id, gen := uuid.NewString(), uuid.New()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cause, cleanup := errors.New("reply lost"), errors.New("cleanup will retry")
	cfg := Config{RunnerID: id, Substrate: failedMutation{runnertest.NewFake(), cause, cancel},
		BeforeCreate:  func(context.Context, runner.SandboxSpec) error { return nil },
		Created:       func(context.Context, runner.SandboxSpec, string) error { return nil },
		CreateFailed:  func(context.Context, runner.SandboxSpec) error { return nil },
		BeforeRefCall: func(context.Context, string, string, uuid.UUID) error { return nil },
		AfterRefCall: func(context.Context, string, string, string, uuid.UUID) error {
			t.Fatal("failed substrate result persisted as success")
			return nil
		},
		ExecutionResult: func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error) {
			return runnerwire.ExecutionResult{}, nil
		},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("missing mutation completion handler accepted")
	}
	calls := 0
	cfg.MutationFinished = func(finish context.Context, method, ref string, runID, generation uuid.UUID, err error) error {
		calls++
		if finish.Err() != nil || ctx.Err() == nil || method != runnerwire.MethodExec || ref != "sandbox" || runID != uuid.Nil || generation != gen || !errors.Is(err, cause) {
			t.Fatal("completion lost detached lifetime or exact failure identity")
		}
		if _, bounded := finish.Deadline(); !bounded {
			t.Fatal("unbounded mutation completion")
		}
		return cleanup
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := runnerwire.Marshal(runnerwire.ExecArgs{Ref: placement.RunnerRefPrefix(id) + "sandbox", ExecutionGeneration: gen})
	if _, err := d.refCall(ctx, runnerwire.MethodExec, args); !errors.Is(err, cause) || !errors.Is(err, cleanup) || calls != 1 {
		t.Fatalf("failure completion: calls=%d err=%v", calls, err)
	}
	cfg.BeforeRefCall = func(context.Context, string, string, uuid.UUID) error { return runner.ErrRunnerOffline }
	d, _ = New(cfg)
	if _, err := d.refCall(t.Context(), runnerwire.MethodExec, args); !errors.Is(err, runner.ErrRunnerOffline) || calls != 1 {
		t.Fatal("refused mutation invoked finish")
	}
}
