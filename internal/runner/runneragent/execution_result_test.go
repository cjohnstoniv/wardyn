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

func TestExecutionResultChecksReferenceAndQueriesWithoutExecution(t *testing.T) {
	for _, mode := range []string{"bound", "ambiguous", "foreign", "missing generation", "stale generation"} {
		t.Run(mode, func(t *testing.T) {
			id, generation := uuid.NewString(), uuid.New()
			fake := runnertest.NewFake()
			queries := 0
			ref := placement.RunnerRefPrefix(id) + "sandbox"
			if mode == "foreign" {
				ref = placement.RunnerRefPrefix(uuid.NewString()) + "sandbox"
			}
			requested := generation
			if mode == "missing generation" {
				requested = uuid.Nil
			}
			if mode == "stale generation" {
				requested = uuid.New()
			}
			missing := errors.New("no matching durable intent")
			d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake,
				BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
				Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
				ExecutionResult: func(_ context.Context, local string, got uuid.UUID) (runnerwire.ExecutionResult, error) {
					queries++
					if local != "sandbox" {
						t.Fatal("query changed local reference")
					}
					if got != generation {
						return runnerwire.ExecutionResult{}, missing
					}
					handle := "exec-1"
					if mode == "ambiguous" {
						handle = ""
					}
					return runnerwire.ExecutionResult{Generation: got, Operation: runnerwire.MethodExec, ExecID: handle, Attempted: true}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			org, local := runnerwire.Loopback()
			defer org.Close()
			peer := runnerwire.NewPeer(local, runnerwire.PeerConfig{})
			defer peer.Close()
			data, _ := runnerwire.Marshal(runnerwire.ExecutionResultArgs{Ref: ref, Generation: requested})
			result, err := d.Call(t.Context(), peer, 2, runnerwire.MethodExecutionResult, data)
			if len(fake.Calls) != 0 || len(fake.Created) != 0 {
				t.Fatal("read-only recovery reached substrate side effects")
			}
			switch mode {
			case "bound", "ambiguous":
				got, ok := result.(runnerwire.ExecutionResult)
				if err != nil || !ok || !got.Attempted || got.Generation != generation || queries != 1 || (got.ExecID == "") != (mode == "ambiguous") {
					t.Fatalf("durable result lost: %+v %v", result, err)
				}
			case "stale generation":
				if !errors.Is(err, missing) || queries != 1 {
					t.Fatal("stale generation not checked against intent", err)
				}
			default:
				if err == nil || queries != 0 {
					t.Fatal("unbound query reached durable state", err)
				}
			}
		})
	}
}
