// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

type rollbackSubstrate struct {
	*runnertest.Fake
	killErr error
	killed  bool
}

func (s *rollbackSubstrate) KillSandbox(ctx context.Context, ref string) error {
	s.killed = true
	if s.killErr != nil {
		return s.killErr
	}
	return s.Fake.KillSandbox(ctx, ref)
}

func TestDispatcherForgetsOutputOnlyAfterConfirmedRollback(t *testing.T) {
	for _, stage := range []string{"created", "bind"} {
		for _, cleanup := range []string{"success", "failure"} {
			t.Run(stage+"/"+cleanup, func(t *testing.T) {
				failure := errors.New("binding persistence failed")
				sub := &rollbackSubstrate{Fake: runnertest.NewFake()}
				if cleanup == "failure" {
					sub.killErr = errors.New("sandbox cleanup failed")
				}
				id, run := uuid.NewString(), uuid.New()
				forgotten, contained := 0, 0
				d, err := newTestDispatcher(Config{
					RunnerID: id, Substrate: sub,
					BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
					Created: func(context.Context, runner.SandboxSpec, string) error {
						if stage == "created" {
							return failure
						}
						return nil
					},
					CreateFailed: func(context.Context, runner.SandboxSpec) error { contained++; return nil },
					Output:       func(uuid.UUID) (io.Writer, error) { return io.Discard, nil },
					BindOutput: func(uuid.UUID, string) error {
						if stage != "bind" {
							t.Fatal("bound output after lifecycle persistence failed")
						}
						return failure
					},
					ForgetOutput: func(got uuid.UUID) error {
						if got != run || !sub.killed || sub.killErr != nil {
							t.Fatal("deleted output without confirmed rollback of this run")
						}
						forgotten++
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
				data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: run, RunnerID: id}, ExecOutput: true})
				_, err = d.Call(context.Background(), peer, 2, runnerwire.MethodCreateSandbox, data)
				if !errors.Is(err, failure) || !sub.killed || contained != 1 {
					t.Fatalf("lost original error or rollback attempt: %v", err)
				}
				if sub.killErr != nil && !errors.Is(err, sub.killErr) {
					t.Fatalf("lost cleanup error: %v", err)
				}
				want := 0
				if sub.killErr == nil {
					want = 1
				}
				if forgotten != want {
					t.Fatalf("forgotten=%d, want %d", forgotten, want)
				}
			})
		}
	}
}
