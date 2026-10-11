// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type executionTransport struct {
	online bool
	calls  int
	args   any
	result runnerwire.ExecutionResult
}

func (t *executionTransport) Online() bool     { return t.online }
func (*executionTransport) SetHandler(Handler) {}
func (*executionTransport) TakeStream(uint32) (*runnerwire.Stream, error) {
	return nil, runnerwire.ErrNoStream
}
func (t *executionTransport) Call(_ context.Context, _ string, args, result any, _ ...runnerwire.CallOption) error {
	t.calls++
	t.args = args
	if exec, ok := result.(*runnerwire.ExecResult); ok {
		exec.ExecID = "exec"
	}
	if recovered, ok := result.(*runnerwire.ExecutionResult); ok {
		*recovered = t.result
	}
	return nil
}

func TestRemoteExecutionResultRejectsMismatchedDurableReply(t *testing.T) {
	for _, mode := range []string{"bound", "ambiguous", "wrong generation", "unattempted", "wrong operation", "foreign", "missing generation"} {
		t.Run(mode, func(t *testing.T) {
			id, generation := uuid.NewString(), uuid.New()
			transport := &executionTransport{online: true, result: runnerwire.ExecutionResult{Generation: generation, Operation: runnerwire.MethodStart, Attempted: true, ExecID: "main"}}
			sub := New(id, transport, Options{})
			ref := placement.RunnerRefPrefix(id) + "sandbox"
			switch mode {
			case "ambiguous":
				transport.result.ExecID = ""
			case "wrong generation":
				transport.result.Generation = uuid.New()
			case "unattempted":
				transport.result.Attempted = false
			case "wrong operation":
				transport.result.Operation = runnerwire.MethodKill
			case "foreign":
				ref = placement.RunnerRefPrefix(uuid.NewString()) + "sandbox"
			case "missing generation":
				generation = uuid.Nil
			}
			result, err := sub.ExecutionResult(t.Context(), ref, generation)
			if mode == "bound" || mode == "ambiguous" {
				if err != nil || result != transport.result {
					t.Fatalf("lost durable result %+v %v", result, err)
				}
				args, ok := transport.args.(runnerwire.ExecutionResultArgs)
				if !ok || args.Ref != ref || args.Generation != generation {
					t.Fatal("query identity was not preserved")
				}
			} else if err == nil {
				t.Fatal("mismatched/unbound execution result accepted")
			}
			if (mode == "foreign" || mode == "missing generation") && transport.calls != 0 {
				t.Fatal("unbound recovery query reached CALL")
			}
		})
	}
}

type executionContextKey struct{}

func TestRemoteExecutionUsesExactPersistedDispatchContext(t *testing.T) {
	for _, method := range []string{runnerwire.MethodExec, runnerwire.MethodStart} {
		for _, mode := range []string{"bound", "missing context", "callback error", "zero", "missing hook", "foreign ref", "offline"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				id, generation := uuid.NewString(), uuid.New()
				transport := &executionTransport{online: mode != "offline"}
				ref := placement.RunnerRefPrefix(id) + "sandbox"
				if mode == "foreign ref" {
					ref = placement.RunnerRefPrefix(uuid.NewString()) + "sandbox"
				}
				lookup := 0
				failure := errors.New("persisted operation unavailable")
				opts := Options{ExecutionGeneration: func(ctx context.Context, m, r string) (uuid.UUID, error) {
					lookup++
					if m != method || r != ref {
						t.Fatal("dispatch identity changed")
					}
					if mode == "callback error" || ctx.Value(executionContextKey{}) != generation {
						return uuid.Nil, failure
					}
					if mode == "zero" {
						return uuid.Nil, nil
					}
					return generation, nil
				}}
				if mode == "missing hook" {
					opts.ExecutionGeneration = nil
				}
				sub := New(id, transport, opts)
				sub.requireExecutionGeneration = true
				ctx := t.Context()
				if mode != "missing context" {
					ctx = context.WithValue(ctx, executionContextKey{}, generation)
				}
				var err error
				if method == runnerwire.MethodExec {
					_, err = sub.Exec(ctx, ref, []string{"agent"})
				} else {
					err = sub.StartSandbox(ctx, ref)
				}
				if mode != "bound" {
					if err == nil || transport.calls != 0 {
						t.Fatalf("unbound dispatch reached CALL: calls=%d err=%v", transport.calls, err)
					}
					if (mode == "callback error" || mode == "missing context") && !errors.Is(err, failure) {
						t.Fatal("persisted operation error lost", err)
					}
					if (mode == "foreign ref" || mode == "offline" || mode == "missing hook") && lookup != 0 {
						t.Fatal("generation lookup preceded reference/session gate")
					}
					return
				}
				if err != nil || transport.calls != 1 || lookup != 1 {
					t.Fatalf("bound dispatch: calls=%d lookup=%d err=%v", transport.calls, lookup, err)
				}
				var got uuid.UUID
				switch args := transport.args.(type) {
				case runnerwire.ExecArgs:
					got = args.ExecutionGeneration
				case runnerwire.RefArgs:
					got = args.ExecutionGeneration
				default:
					t.Fatalf("unexpected CALL payload %T", args)
				}
				if got != generation {
					t.Fatalf("CALL generation %s want immutable dispatch %s", got, generation)
				}
			})
		}
	}
}
