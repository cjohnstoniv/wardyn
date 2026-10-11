// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func failStateSave(t *testing.T, m *Manager) func() {
	t.Helper()
	p := filepath.Join(m.dir, "offline", "state.tmp")
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "block"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDurableReadsRefuseUnsavedExecution(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := t.Context()
	gen := executionIntent(t, m, id, org, runnerwire.MethodExec)
	restore := failStateSave(t, m)
	defer restore()
	sub.stopErr = errors.New("transient daemon failure")
	if err := m.AfterRefCall(ctx, runnerwire.MethodExec, id.String(), "new-handle", gen); !errors.Is(err, sub.stopErr) {
		t.Fatalf("save/containment error: %v", err)
	}
	if result, err := m.ExecutionResult(ctx, id.String(), gen); err == nil {
		t.Fatalf("unsaved result exposed: %+v", result)
	}
	if rows, err := allState(ctx, m); err == nil {
		t.Fatalf("unsaved STATE exposed: %+v", rows)
	}
	if err := m.BeforeRefCall(ctx, runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
		t.Fatal("mutation admitted with failed persistence")
	}
	restore()
	// A successful unrelated flush must not discharge failed containment.
	if _, err := m.ExecutionResult(ctx, id.String(), gen); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckLapses(ctx); !errors.Is(err, sub.stopErr) {
		t.Fatalf("live lease lost retry: %v", err)
	}
	if sub.stops != 2 {
		t.Fatalf("containment attempts=%d", sub.stops)
	}
	if err := m.BeforeRefCall(ctx, runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
		t.Fatal("flush cleared containment obligation")
	}
}

func TestAmbiguousExecutionRestartContainsDespiteFreshLease(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := t.Context()
	gen := executionIntent(t, m, id, org, runnerwire.MethodExec)
	restore := failStateSave(t, m)
	sub.stopErr = errors.New("daemon unavailable")
	if err := m.AfterRefCall(ctx, runnerwire.MethodExec, id.String(), "lost-handle", gen); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	restore()
	restarted, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Clock(runnerwire.Hello{ServerTime: org}, restarted.now()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.BeforeReady(ctx, &runnerwire.Peer{}, false); !errors.Is(err, sub.stopErr) {
		t.Fatalf("failed startup containment admitted: %v", err)
	}
	sub.stopErr = nil
	if err := restarted.BeforeReady(ctx, &runnerwire.Peer{}, false); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Lease(ctx, restarted.state.Runs[id].Lease); err != nil {
		t.Fatal(err)
	}
	result, err := restarted.ExecutionResult(ctx, id.String(), gen)
	if err != nil || !result.Attempted || result.ExecID != "" {
		t.Fatalf("restart invented durable handle: %+v %v", result, err)
	}
	if err := restarted.BeforeRefCall(ctx, runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
		t.Fatal("fresh lease bypassed ambiguous execution")
	}
	// Successful containment does not authorize recovery or fabricate a handle.
	if restarted.state.Runs[id].MutationFailure == nil {
		t.Fatal("restart lost sticky recovery marker")
	}

}

func TestExitRetryFlushesBeforeSuccessAndReplay(t *testing.T) {
	for _, observe := range []bool{false, true} {
		t.Run(map[bool]string{false: "callback", true: "state"}[observe], func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			id := createLocal(t, m, org)
			ctx := context.Background()
			gen := executionIntent(t, m, id, org, runnerwire.MethodExec)
			if err := m.AfterRefCall(ctx, runnerwire.MethodExec, id.String(), "known-handle", gen); err != nil {
				t.Fatal(err)
			}
			restore := failStateSave(t, m)
			code := 7
			call := func() error { return m.AgentExited(ctx, id, id.String(), code) }
			if observe {
				sub.agent = runner.Status{State: types.RunFailed, ExitCode: &code}
				call = func() error { _, err := allState(ctx, m); return err }
			}
			if err := call(); err == nil {
				t.Fatal("fixture did not fail exit save")
			}
			original := *m.state.Runs[id].Exit
			if err := m.Replay(ctx, nil); err == nil {
				t.Fatal("unsaved exit replay succeeded")
			}
			if err := call(); err == nil {
				t.Fatal("duplicate exit acknowledged without persistence")
			}
			restore()
			if err := call(); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			got := restarted.state.Runs[id].Exit
			if got == nil || *got != original {
				t.Fatalf("successful retry lost/changed exit: %+v", got)
			}
		})
	}
}
