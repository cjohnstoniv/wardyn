// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// P5 (0.7.3 field report): POST /setup/harness-login blocked through a cold
// image pull. The whole launch — CreateRun, the audit stamp, then dispatchRun,
// which blocks on CreateSandbox for as long as the substrate needs (up to
// canaryWaitTimeout ON TOP of a pull; the reporting estate measured 131s) — ran
// inside the request. The console's wfetch deadline is 60s, so the pane showed
// the unreachable-daemon sentence, setRunId never ran, and Cancel had nothing
// to kill: the sandbox was orphaned to harnessLoginIdleCap.
//
// coldPullRunner is that cold pull, made deterministic: CreateSandbox blocks until
// the test releases it. Everything else is fakeRunner's.
type coldPullRunner struct {
	*fakeRunner
	gate chan struct{}
}

func (g *coldPullRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	<-g.gate
	return g.fakeRunner.CreateSandbox(ctx, spec)
}

// waitForSandbox blocks until the launch goroutine has actually composed a
// sandbox spec, so a test that reads lastSandboxEnv reads the launch's answer
// rather than the zero value it holds for the first microseconds after the POST
// is answered. The assertions it guards are unchanged — only their timing
// assumption moved, because the POST no longer waits for dispatch.
func (f *fakeRunner) waitForSandbox(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := f.createCalls
		f.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the login launch never reached CreateSandbox within 5s")
}

// waitForAuditRows polls the recorder for `want` rows of an action, so a test can
// assert on work the detached launch goroutine does after the response.
func waitForAuditRows(t *testing.T, audit *memAudit, action string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(audit.find(action)) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s rows = %d after 5s, want %d", action, len(audit.find(action)), want)
}

func perUserAWSRow() types.AgentProvider {
	return types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
	}
}

// TestHandleHarnessLogin_ReturnsBeforeTheSandboxIsUp is P5's own reproduction:
// with a CreateSandbox that never returns, the route must still answer — with
// the run id the pane needs to poll, attach and cancel — and the launch must
// then finish on its own.
func TestHandleHarnessLogin_ReturnsBeforeTheSandboxIsUp(t *testing.T) {
	gr := &coldPullRunner{fakeRunner: &fakeRunner{}, gate: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(gr.gate)
		}
	})
	srv, audit := perUserLoginSrvWithRunner(t, gr, perUserAWSRow())

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login",
			ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember), `{"provider":"aws"}`)
	}()

	var w *httptest.ResponseRecorder
	select {
	case w = <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("POST /setup/harness-login did not answer within 2s while CreateSandbox blocked — " +
			"the console's own deadline is 60s and a cold pull exceeds it, so the pane never learns the run id")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The id is what makes Cancel work during the pull, and the state is what
	// tells the pane it is not attachable yet.
	var body struct {
		RunID string `json:"run_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if body.RunID == "" {
		t.Fatalf("response %s carries no run_id — the pane has nothing to poll or kill", w.Body.String())
	}
	if body.State != string(types.RunPending) {
		t.Errorf("state = %q, want %q — the run is answered before dispatch, not after it", body.State, types.RunPending)
	}
	// The launch-time stamp is written BEFORE the answer: the upload binds to
	// it, so it can never be a thing the goroutine might not get to.
	if n := len(audit.find("harness.login.started")); n != 1 {
		t.Fatalf("harness.login.started rows = %d at response time, want 1", n)
	}

	released = true
	close(gr.gate)
	waitForAuditRows(t, audit, "run.interactive", 1)
	waitForAuditRows(t, audit, "harness.login.started", 1)
}

// ceilingBlipStore is integStore with two additions the ceiling arm below needs:
// a governance read that starts failing on demand (the shape a store blip has
// between a login run's CreateRun and its dispatch), and a FailureHint the
// double actually keeps instead of discarding.
type ceilingBlipStore struct {
	*integStore
	fail atomic.Bool

	mu   sync.Mutex
	hint map[uuid.UUID]string
}

func (s *ceilingBlipStore) ResolveGovernanceProfile(ctx context.Context, users, groups []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	if s.fail.Load() {
		return nil, "", errors.New("governance store unavailable")
	}
	return s.integStore.ResolveGovernanceProfile(ctx, users, groups)
}

func (s *ceilingBlipStore) SetRunFailureHint(_ context.Context, id uuid.UUID, hint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hint[id] = hint
	return nil
}

func (s *ceilingBlipStore) failureHint(id uuid.UUID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hint[id]
}

// TestHandleHarnessLogin_CeilingErrorAfterCreateFailsTheRun pins the door P5's
// split opened. resolveDispatchCeiling runs AFTER CreateRun and after the audit
// stamp. Synchronously its error was a 500 and the caller knew; detached, a
// bare `return` would leave a 200 already answered, a run PENDING forever with
// no failure_hint for the pane to render and no watcher lease for a reconcile
// sweep to adopt — the orphan P5 exists to end, wearing a different hat.
//
// Driven at the seam the goroutine runs rather than through the route, because
// the route CANNOT reach it: effectiveCeiling memoizes per request (ceilingMemo)
// and launchHarnessLoginRun already resolved it, so a second failure cannot
// appear out of a successful first. The blip is therefore simulated exactly
// where a detached tail would meet one — after the run row exists.
func TestHandleHarnessLogin_CeilingErrorAfterCreateFailsTheRun(t *testing.T) {
	h := newHarness(t)
	spy := &revokeSpy{Provider: h.idp}
	st := &ceilingBlipStore{
		integStore: &integStore{govEscapeStore: newGovEscapeStore(&capStore{}), site: agentRoster(perUserAWSRow())},
		hint:       map[uuid.UUID]string{},
	}
	rnr := &fakeRunner{}
	cfg := baseTestConfig(h, st)
	cfg.Identity = spy
	cfg.Audit = &memAudit{}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = rnr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)

	// A member context with a usable group snapshot: the ceiling resolves
	// through the ordinary lane, so flipping the store below is the only thing
	// that changes between the two resolutions.
	ctx := withOIDCGroups(withOIDCRole(withOIDCHuman(context.Background(), "sub-member"), oidc.RoleMember), []string{"eng"})
	ctx = withOIDCEmail(ctx, "member@corp.example")
	hl, ok := agentHarnessLogin(awsSSOAgent)
	if !ok {
		t.Fatal("aws-sso harness login convention missing")
	}
	run, dispatch, err := srv.launchHarnessLoginRun(ctx, "member@corp.example", hl, perUserPortal, awsSSOPin{}, awsSSOScope{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if run.State != types.RunPending {
		t.Fatalf("run state after launch = %q, want PENDING (dispatch has not run yet)", run.State)
	}

	st.fail.Store(true)
	srv.finishHarnessLoginLaunch(ctx, run, dispatch)

	got, gerr := st.GetRun(context.Background(), run.ID)
	if gerr != nil {
		t.Fatalf("get run: %v", gerr)
	}
	if got.State != types.RunFailed {
		t.Errorf("run state = %q, want FAILED — a 200 is already answered, so the RUN has to carry the failure", got.State)
	}
	if hint := st.failureHint(run.ID); hint != harnessLoginCeilingUnresolved {
		t.Errorf("failure_hint = %q, want the lane's own sentence %q", hint, harnessLoginCeilingUnresolved)
	}
	if len(spy.revoked) != 1 || spy.revoked[0] != run.ID {
		t.Errorf("revoked = %v, want exactly this run's identity — a run that never launched must not keep a mintable token", spy.revoked)
	}
	if rnr.createCalls != 0 {
		t.Errorf("CreateSandbox calls = %d, want 0 — nothing may be provisioned under an unresolved ceiling", rnr.createCalls)
	}
}
