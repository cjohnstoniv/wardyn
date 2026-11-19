// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// confirmStore serves one KILLED run and, like store.PG, lets the kill clear its sandbox ref.
type confirmStore struct {
	store.Store
	mu  sync.Mutex
	run types.AgentRun
}

func (s *confirmStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run, nil
}

func (s *confirmStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, _ types.RunState) (bool, error) {
	return from == s.run.State, nil
}

func (s *confirmStore) ClearSandboxRefRetaining(context.Context, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run.SandboxRef = ""
	return nil
}

func (s *confirmStore) ref() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run.SandboxRef
}

// confirmRunner answers KillSandbox from a script, then with last for every call after it.
type confirmRunner struct {
	*fakeRunner
	mu     sync.Mutex
	script []error
	last   error
	calls  int
}

func (r *confirmRunner) KillSandbox(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if len(r.script) > 0 {
		err := r.script[0]
		r.script = r.script[1:]
		return err
	}
	return r.last
}

// release makes every later KillSandbox answer nil.
func (r *confirmRunner) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.script, r.last = nil, nil
}

var errStillTerminating = fmt.Errorf("k8s: teardown: %w: 2 pods terminating", runner.ErrTeardownConfirming)

type confirmRig struct {
	h     *harness
	srv   *Server
	st    *confirmStore
	rr    *confirmRunner
	runID uuid.UUID
}

func newConfirmRig(t *testing.T, window time.Duration, script []error, last error) *confirmRig {
	t.Helper()
	prevWindow, prevRetry := killConfirmWindow, killConfirmRetry
	killConfirmWindow, killConfirmRetry = window, 5*time.Millisecond
	t.Cleanup(func() { killConfirmWindow, killConfirmRetry = prevWindow, prevRetry })
	h := newHarness(t)
	runID := uuid.New()
	st := &confirmStore{run: types.AgentRun{ID: runID, State: types.RunKilled, SandboxRef: "sbx-1"}}
	rr := &confirmRunner{fakeRunner: &fakeRunner{}, script: script, last: last}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rr
	return &confirmRig{h: h, srv: New(cfg), st: st, rr: rr, runID: runID}
}

func (r *confirmRig) kill(t *testing.T) map[string]any {
	t.Helper()
	w := do(t, r.srv, http.MethodPost, "/api/v1/runs/"+r.runID.String()+"/kill", adminToken, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("kill = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("kill body: %v", err)
	}
	return body
}

func (r *confirmRig) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range r.h.audit.snapshot() {
		if ev.Action == action && ev.RunID != nil && *ev.RunID == r.runID {
			out = append(out, ev)
		}
	}
	return out
}

// settle waits for the confirm to end and returns its rows.
func (r *confirmRig) settle(t *testing.T) []types.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, running := killConfirms.Load(r.runID); !running && len(r.rows("run.kill.confirm")) > 0 {
			return r.rows("run.kill.confirm")
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the background confirm did not end with a row")
	return nil
}

// TestKillRun_ConfirmingStopAnswers202ThenConfirms: a kill whose pods are still terminating answers 202
// with `teardown: confirming` (no failure row, no 500), keeps the ref, and the background confirm writes
// one run.kill.confirm success row and clears the ref once the stop answers nil.
func TestKillRun_ConfirmingStopAnswers202ThenConfirms(t *testing.T) {
	r := newConfirmRig(t, 5*time.Second, []error{errStillTerminating, errStillTerminating}, nil)

	body := r.kill(t)
	if body["state"] != string(types.RunKilled) || body["teardown"] != "confirming" {
		t.Fatalf("kill body = %v, want state KILLED and teardown confirming", body)
	}
	rows := r.settle(t)
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.kill.confirm rows = %+v, want exactly one success row", rows)
	}
	if got := r.st.ref(); got != "" {
		t.Errorf("sandbox ref after a confirmed stop = %q, want cleared", got)
	}
	if got := r.rr.calls; got != 3 {
		t.Errorf("KillSandbox calls = %d, want 3 (the kill, then two confirm tries)", got)
	}
	if n := len(r.rows("run.revoke")); n != 0 {
		t.Errorf("run.revoke rows = %d, want 0: a stop in progress is not a failed step", n)
	}
	kills := r.rows("run.kill")
	if len(kills) != 1 || kills[0].Outcome != "success" || !confirmDataHas(kills[0].Data, "sandbox_stop", "confirming") {
		t.Errorf("run.kill rows = %+v, want one success row with sandbox_stop confirming", kills)
	}
}

// TestKillRun_ConfirmingForeverWritesOneFailureAtTheBound: a stop that never confirms ends in one failure
// row naming the step, and the ref stays for the boot pass and a repeat of the kill.
func TestKillRun_ConfirmingForeverWritesOneFailureAtTheBound(t *testing.T) {
	r := newConfirmRig(t, 150*time.Millisecond, nil, errStillTerminating)

	if body := r.kill(t); body["teardown"] != "confirming" {
		t.Fatalf("kill body = %v, want teardown confirming", body)
	}
	rows := r.settle(t)
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("run.kill.confirm rows = %+v, want exactly one failure row", rows)
	}
	if !confirmDataHas(rows[0].Data, "step", "kill_sandbox") || !confirmDataHas(rows[0].Data, "reason", auditStepKillSandbox) {
		t.Errorf("failure row data = %s, want step kill_sandbox and its reason", rows[0].Data)
	}
	if got := r.st.ref(); got != "sbx-1" {
		t.Errorf("sandbox ref after an unconfirmed stop = %q, want it kept", got)
	}
}

// TestKillRun_SecondKillDuringConfirmStartsNoSecondLoop: a second kill while the confirm runs answers the
// same 202 and starts nothing; the confirm ends in one row, not two. The window is long and the stop is
// released only after the second kill, so the first loop is certainly still running when it arrives.
func TestKillRun_SecondKillDuringConfirmStartsNoSecondLoop(t *testing.T) {
	r := newConfirmRig(t, 30*time.Second, nil, errStillTerminating)

	first, second := r.kill(t), r.kill(t)
	if first["teardown"] != "confirming" || second["teardown"] != "confirming" {
		t.Fatalf("kill bodies = %v and %v, want both teardown confirming", first, second)
	}
	r.rr.release()
	r.settle(t)
	r.srv.WaitBackground()
	if rows := r.rows("run.kill.confirm"); len(rows) != 1 {
		t.Errorf("run.kill.confirm rows = %d, want 1: a second kill started a second loop", len(rows))
	}
}

func confirmDataHas(data []byte, key, want string) bool {
	var m map[string]any
	return json.Unmarshal(data, &m) == nil && m[key] == want
}

// TestKillTeardownTail_NoConfirmCarrierKeepsTheFailure: only a person's kill asks for the background
// confirm. Any other kill (the lifetime kill, the sweep's recovered tail, a workload or SCIM end) whose
// stop answers ErrTeardownConfirming still records the failed step and a failure row, and sets no
// sandbox_stop, so its caller's failure handling and the sweep's retry stay as they were.
func TestKillTeardownTail_NoConfirmCarrierKeepsTheFailure(t *testing.T) {
	r := newConfirmRig(t, 5*time.Second, nil, errStillTerminating)

	data := r.srv.killTeardownTail(context.Background(), r.st.run, types.ActorSystem, "test", nil, true)

	if data["runner_error"] == nil {
		t.Errorf("killData = %v, want the runner_error failure for a kill with no confirm carrier", data)
	}
	kills := r.rows("run.kill")
	if len(kills) != 1 || kills[0].Outcome != "failure" || confirmDataHas(kills[0].Data, "sandbox_stop", "confirming") {
		t.Errorf("run.kill rows = %+v, want one failure row with no sandbox_stop confirming", kills)
	}
	if got := r.st.ref(); got != "sbx-1" {
		t.Errorf("sandbox ref = %q, want it kept", got)
	}
	if _, running := killConfirms.Load(r.runID); running || len(r.rows("run.kill.confirm")) != 0 {
		t.Error("a background confirm started for a kill that did not ask for one")
	}
}
