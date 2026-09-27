// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ctxBlockingCreateRunner is a create that only a cancelled context ends — an
// agent pod left Unschedulable for the whole readiness wait. The FIRST
// CreateSandbox blocks; any later one (a superseding launch) is fakeRunner's.
// release unblocks a create the fix never cancelled, so a red run still ends.
type ctxBlockingCreateRunner struct {
	*fakeRunner
	entered, release chan struct{}
	gotErr           chan error
	once             sync.Once
}

func newCtxBlockingCreateRunner(t *testing.T) *ctxBlockingCreateRunner {
	r := &ctxBlockingCreateRunner{
		fakeRunner: &fakeRunner{},
		entered:    make(chan struct{}), release: make(chan struct{}), gotErr: make(chan error, 1),
	}
	t.Cleanup(func() { close(r.release) })
	return r
}

func (r *ctxBlockingCreateRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	first := false
	r.once.Do(func() { first = true })
	if !first {
		return r.fakeRunner.CreateSandbox(ctx, spec)
	}
	close(r.entered)
	select {
	case <-ctx.Done():
		r.gotErr <- ctx.Err()
		return runner.Sandbox{}, ctx.Err()
	case <-r.release:
		return runner.Sandbox{}, errors.New("released by test cleanup")
	}
}

func (r *ctxBlockingCreateRunner) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-r.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch never reached CreateSandbox within 5s")
	}
}

// waitCancelled asserts the blocked create saw its context cancelled within 3s.
func (r *ctxBlockingCreateRunner) waitCancelled(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateSandbox ended with %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the killed run's CreateSandbox was still running 3s after the kill — it holds its pods " +
			"(and the node's room) until its own readiness wait expires, which is what starved the retry in #1182")
	}
}

// TestKillOfStartingRunCancelsItsCreate is #1182's fix: a kill of a STARTING
// run stops that run's in-flight CreateSandbox, so the driver's own rollback
// removes its pods now rather than after canaryWaitTimeout. A STARTING run has
// no sandbox_ref, so the kill's teardown tail cannot reach the sandbox; the
// create's cancellation is the only thing that can.
//
// Red on the unfixed tree: dispatch hands CreateSandbox a context nothing
// cancels, and the create runs on after the 202.
func TestKillOfStartingRunCancelsItsCreate(t *testing.T) {
	rn := newCtxBlockingCreateRunner(t)
	srv, st, _, run := dispatchTeardownFixture(t, rn, types.RunPending)

	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest",
			Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
		})
	}()
	rn.waitEntered(t)
	if got := st.State(); got != types.RunStarting {
		t.Fatalf("state inside CreateSandbox = %s, want STARTING", got)
	}

	if w := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", adminToken, ""); w.Code != http.StatusAccepted {
		t.Fatalf("kill: code = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	rn.waitCancelled(t)

	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return after its create was cancelled")
	}
	if got := st.State(); got != types.RunKilled {
		t.Fatalf("state after the cancelled create = %s, want KILLED — the create's failure arm must not overwrite the kill", got)
	}
}
