// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

func TestDispatcherCarriesExplicitExecutionGeneration(t *testing.T) {
	for _, method := range []string{runnerwire.MethodExec, runnerwire.MethodStart} {
		for _, supplied := range []string{"current", "stale", "missing"} {
			t.Run(method+"/"+supplied, func(t *testing.T) {
				ctx := t.Context()
				fake := runnertest.NewFake()
				sb, err := fake.CreateSandbox(ctx, runner.SandboxSpec{RunID: uuid.New()})
				if err != nil {
					t.Fatal(err)
				}
				id, generation := uuid.NewString(), uuid.New()
				got := generation
				if supplied == "stale" {
					got = uuid.New()
				}
				if supplied == "missing" {
					got = uuid.Nil
				}
				guard := errors.New("execution generation mismatch")
				before, after := 0, 0
				d, err := New(Config{RunnerID: id, Substrate: fake,
					BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
					Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
					CreateFailed: func(context.Context, runner.SandboxSpec) error { return nil },
					ExecutionResult: func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error) {
						return runnerwire.ExecutionResult{}, runnerwire.Refuse("not queried in this fixture")
					},
					BeforeRefCall: func(_ context.Context, m, ref string, value uuid.UUID) error {
						before++
						if m != method || ref != sb.Ref || value != got {
							t.Fatal("CALL generation was replaced by another identity")
						}
						if value != generation {
							return guard
						}
						return nil
					},
					MutationFinished: func(context.Context, string, string, uuid.UUID, uuid.UUID, error) error { return nil },
					AfterRefCall: func(_ context.Context, m, ref, execID string, value uuid.UUID) error {
						after++
						if m != method || ref != sb.Ref || value != generation || (method == runnerwire.MethodExec && execID == "") {
							t.Fatal("durable generation or execution handle lost")
						}
						return nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				org, local := runnerwire.Loopback()
				defer org.Close()
				peer := runnerwire.NewPeer(local, runnerwire.PeerConfig{})
				defer peer.Close()
				ref := placement.RunnerRefPrefix(id) + sb.Ref
				data, _ := runnerwire.Marshal(runnerwire.ExecArgs{Ref: ref, Argv: []string{"agent"}, ExecutionGeneration: got})
				if method == runnerwire.MethodStart {
					data, _ = runnerwire.Marshal(runnerwire.RefArgs{Ref: ref, ExecutionGeneration: got})
				}
				_, err = d.Call(ctx, peer, 2, method, data)
				if before != 1 {
					t.Fatal("execution guard was bypassed")
				}
				if supplied == "current" {
					if err != nil || after != 1 || !fake.Called(method, sb.Ref) {
						t.Fatalf("current generation failed: after=%d err=%v", after, err)
					}
				} else if !errors.Is(err, guard) || after != 0 || fake.Called(method, sb.Ref) {
					t.Fatalf("stale execution reached substrate: after=%d err=%v", after, err)
				}
			})
		}
	}
}

func TestContainmentNeverBorrowsExecutionGeneration(t *testing.T) {
	for _, method := range []string{runnerwire.MethodStop, runnerwire.MethodKill, runnerwire.MethodEnd, runnerwire.MethodStopProxy, runnerwire.MethodReplaceProxy} {
		data, _ := runnerwire.Marshal(runnerwire.RefArgs{Ref: "local", ExecutionGeneration: uuid.New()})
		generation, err := executionGeneration(method, data)
		if err != nil || generation != uuid.Nil {
			t.Fatalf("%s inferred execution authority: %s %v", method, generation, err)
		}
	}
}
