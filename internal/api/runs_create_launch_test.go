// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// waitForCreates is waitForSandbox for the nth CreateSandbox call, for tests
// that POST more than one run and read the last spec each time.
func (f *fakeRunner) waitForCreates(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "CreateSandbox call", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.createCalls >= n
	})
}

// waitForRunState polls the store until the run reaches want: POST /runs
// answers PENDING and the launch moves the run on after the response.
func waitForRunState(t *testing.T, srv *Server, id uuid.UUID, want types.RunState) {
	t.Helper()
	waitFor(t, "run state "+string(want), func() bool {
		got, err := srv.cfg.Store.GetRun(context.Background(), id)
		return err == nil && got.State == want
	})
}

// lockedRevokeSpy records revocations the detached launch makes while the
// test is reading.
type lockedRevokeSpy struct {
	revokeSpy
	mu sync.Mutex
}

func (r *lockedRevokeSpy) RevokeRun(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revokeSpy.RevokeRun(ctx, id)
}

func (r *lockedRevokeSpy) revokedIDs() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.revoked)
}

// createLaunchFixture is a Server that really creates AND dispatches a run on
// rnr, over a store that keeps failure hints.
func createLaunchFixture(t *testing.T, rnr runner.Runner) (*Server, *ceilingBlipStore, *memAudit, *lockedRevokeSpy) {
	t.Helper()
	h := newHarness(t)
	spy := &lockedRevokeSpy{revokeSpy: revokeSpy{Provider: h.idp}}
	st := &ceilingBlipStore{
		integStore: &integStore{govEscapeStore: newGovEscapeStore(&capStore{})},
		hint:       map[uuid.UUID]string{},
	}
	audit := &memAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Identity = spy
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Runner = rnr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), st, audit, spy
}

// TestCreateRun_ReturnsBeforeTheSandboxIsUp: with a CreateSandbox that does not
// return, POST /runs must still answer 201 with the run id — the console's
// launch deadline is 5 minutes and an image build alone may take 30 — and the
// launch must then carry on server-side and finish on its own.
func TestCreateRun_ReturnsBeforeTheSandboxIsUp(t *testing.T) {
	gr := &coldPullRunner{fakeRunner: &fakeRunner{}, gate: make(chan struct{}), entered: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(gr.gate)
		}
	})
	srv, _, _, _ := createLaunchFixture(t, gr)
	member := govSession(t, "sub-member", []string{"eng"}, false)

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, `{"agent":"claude-code","task":"t"}`)
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("POST /runs did not answer within 2s while CreateSandbox blocked — the caller never learns the run id")
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var body createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if body.ID == uuid.Nil {
		t.Fatalf("response %s carries no run id", w.Body.String())
	}
	if want := "/api/v1/runs/" + body.ID.String(); w.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	if body.State != types.RunPending {
		t.Errorf("state = %q, want %q — the run is answered before dispatch, not after it", body.State, types.RunPending)
	}

	// The launch continues after the answer: it reaches the pull with the gate
	// still shut, and the run is not RUNNING until the pull finishes.
	select {
	case <-gr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached launch never reached CreateSandbox after the 201")
	}
	if got, _ := srv.cfg.Store.GetRun(context.Background(), body.ID); got.State != types.RunStarting {
		t.Errorf("state during the pull = %q, want %q", got.State, types.RunStarting)
	}

	released = true
	close(gr.gate)
	waitForRunState(t, srv, body.ID, types.RunRunning)
}

// TestCreateRun_DispatchPanicFailsTheRun: a panic in the detached launch must
// not take the daemon down, and must leave the run FAILED with a hint and its
// identity revoked — the caller already holds a 201, so the run itself is the
// only place the failure can be read.
func TestCreateRun_DispatchPanicFailsTheRun(t *testing.T) {
	srv, st, audit, spy := createLaunchFixture(t, &panicRunner{fakeRunner: &fakeRunner{}})
	member := govSession(t, "sub-member", []string{"eng"}, false)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a launch panic after the run exists is the run's failure, not the request's; body=%s",
			w.Code, w.Body.String())
	}
	var body createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}

	// Revocation is failAndRevoke's last write, so everything else is in place.
	waitFor(t, "the panicked run's identity revocation", func() bool {
		return slices.Contains(spy.revokedIDs(), body.ID)
	})
	got, err := st.GetRun(context.Background(), body.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != types.RunFailed {
		t.Errorf("run state = %q, want FAILED — a panicked launch must not leave a non-terminal run holding an identity", got.State)
	}
	if hint := st.failureHint(body.ID); hint != createRunInternalError {
		t.Errorf("failure_hint = %q, want %q", hint, createRunInternalError)
	}
	if n := len(audit.find("run.dispatch")); n < 1 {
		t.Errorf("run.dispatch failure rows = %d, want at least 1 — a panicked launch must leave a trail", n)
	}
}
