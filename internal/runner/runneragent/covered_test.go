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

// coveredCall sends one ref verb for a sandbox this runner holds, through a dispatcher whose Config
// carries covered, and answers the engine fake, the local ref and the call's error.
func coveredCall(t *testing.T, method string, covered func(string) bool) (*runnertest.Fake, string, error) {
	t.Helper()
	runnerID := uuid.NewString()
	fake := runnertest.NewFake()
	sb, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: fake,
		HoldsSandbox: func(ref string) bool { return ref == sb.Ref },
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
		Covered:      covered,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	t.Cleanup(func() { _ = b.Close() })
	peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	t.Cleanup(peer.Close)
	data, _ := runnerwire.Marshal(runnerwire.RefArgs{Ref: placement.RunnerRefPrefix(runnerID) + sb.Ref})
	_, err = d.Call(t.Context(), peer, 1, method, data)
	return fake, sb.Ref, err
}

// coveredReads are the two read verbs Covered guards, with the name the engine fake records each under.
var coveredReads = []struct{ method, call string }{
	{runnerwire.MethodStatus, "status"},
	{runnerwire.MethodAgentStatus, "agent_status "},
}

// TestDispatcherReadsOfAnUncoveredSandboxAreUnobservable pins A2 (#3667 F1): Status and AgentStatus of
// a sandbox the substrate cannot observe now (macOS VMs off) answer an internal error, which the org
// reads as unknown, and never reach the engine, whose "not found" the org would read as gone.
func TestDispatcherReadsOfAnUncoveredSandboxAreUnobservable(t *testing.T) {
	for _, v := range coveredReads {
		t.Run(v.method, func(t *testing.T) {
			fake, ref, err := coveredCall(t, v.method, func(string) bool { return false })
			if err == nil {
				t.Fatal("an uncovered sandbox's read answered nil; want an internal error")
			}
			if code := runnerwire.ErrorFor(err).Code; code != runnerwire.CodeInternal {
				t.Fatalf("wire code = %q (%v); want %q", code, err, runnerwire.CodeInternal)
			}
			if errors.Is(err, runner.ErrSandboxGone) || errors.Is(err, runner.ErrRunnerOffline) {
				t.Fatalf("%v reads as gone or offline; want unknown", err)
			}
			if fake.Called(v.call, ref) {
				t.Fatalf("an uncovered sandbox's read reached the engine: %v", fake.Calls)
			}
		})
	}
}

// TestDispatcherReachesTheEngineForACoveredSandbox pins the guard's scope: a nil Covered, or one that
// covers the ref, leaves both reads to the engine, and an uncovered ref's stop still reaches it.
func TestDispatcherReachesTheEngineForACoveredSandbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered func(string) bool
	}{
		{name: "nil", covered: nil},
		{name: "covered", covered: func(string) bool { return true }},
	} {
		for _, v := range coveredReads {
			t.Run(tc.name+"/"+v.method, func(t *testing.T) {
				fake, ref, err := coveredCall(t, v.method, tc.covered)
				if err != nil {
					t.Fatalf("a covered sandbox's read answered %v; want nil", err)
				}
				if !fake.Called(v.call, ref) {
					t.Fatalf("a covered sandbox's read did not reach the engine: %v", fake.Calls)
				}
			})
		}
	}
	t.Run("uncovered/stop", func(t *testing.T) {
		fake, ref, err := coveredCall(t, runnerwire.MethodStop, func(string) bool { return false })
		if err != nil {
			t.Fatalf("an uncovered sandbox's stop answered %v; want nil", err)
		}
		if !fake.Called("stop", ref) {
			t.Fatalf("an uncovered sandbox's stop did not reach the engine: %v", fake.Calls)
		}
	})
}
