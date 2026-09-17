// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Finding 7 (0.7.4 field report): a legitimate retry after an aborted sign-in is
// told "The sandbox reported a capture the server does not have — sign in
// again". The mechanism is the ABORTED run outliving the retry: nothing ends it
// (the pane has no unmount cleanup, and its Try again arm does not kill), it
// lives to harnessLoginIdleCap with `aws sso login` still polling, and since the
// image self-runs the pair it completes UNATTENDED — its late, legitimate PUT
// then leaves a row stamped with the OLD run's id, which is exactly what
// serverConfirmsCapture refuses.
//
// The fix: a new sign-in supersedes the caller's older ones, server-side, before
// the new run is created. These cases pin it.

// supersedeStore is the login fixture's store plus the two reads the supersede
// path makes: the seam query (store.ActiveRunsByCreatorReader) and the quota
// count, both answered from the runs this fixture ACTUALLY created rather than
// from canned numbers — the quota case is only meaningful end to end.
type supersedeStore struct {
	*integStore
}

func (s *supersedeStore) ActiveRunsByCreator(_ context.Context, createdBy, task, agent string) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.AgentRun
	for id, r := range s.runs {
		if r.CreatedBy == createdBy && r.Task == task && r.Agent == agent && !s.states[id].IsTerminal() {
			r.State = s.states[id]
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *supersedeStore) CountActiveRunsBy(_ context.Context, createdBy string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.runs {
		if r.CreatedBy == createdBy && !s.states[id].IsTerminal() {
			n++
		}
	}
	return n, nil
}

// QueryAuditEvents: the RUN READ path asks for a run's events to project its UI
// apps (effectiveUIApps, runs_policy.go), and an unimplemented promoted method
// on a double is a nil-pointer panic rather than the logged error that read path
// is written to tolerate. Empty is what a run with no ui.open rows really has.
func (s *supersedeStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

func (s *supersedeStore) stateOf(t *testing.T, id string) types.RunState {
	t.Helper()
	rid, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parse run id %q: %v", id, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[rid]
}

// seed puts a run into the store directly — an orphan that belongs to someone
// else, or to a lane the supersede must not touch.
func (s *supersedeStore) seed(run types.AgentRun) types.AgentRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	run.State = types.RunRunning
	s.runs[run.ID] = run
	s.states[run.ID] = types.RunRunning
	return run
}

func supersedeLoginSrv(t *testing.T, cs *capStore, rnr runner.Runner) (*Server, *memAudit, *supersedeStore) {
	t.Helper()
	if cs == nil {
		cs = &capStore{}
	}
	if rnr == nil {
		rnr = &fakeRunner{}
	}
	h := newHarness(t)
	audit := &memAudit{}
	st := &supersedeStore{integStore: &integStore{
		govEscapeStore: newGovEscapeStore(cs),
		site:           agentRoster(perUserAWSRow()),
	}}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = rnr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), audit, st
}

func memberLoginSession(t *testing.T) *http.Cookie {
	t.Helper()
	return ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
}

// launchLoginRun POSTs one sign-in and returns its run id.
func launchLoginRun(t *testing.T, srv *Server, sess *http.Cookie) string {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", sess, `{"provider":"aws"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /setup/harness-login: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode launch response: %v (%s)", err, w.Body.String())
	}
	if body.RunID == "" {
		t.Fatal("launch answered no run id")
	}
	return body.RunID
}

// waitRunState waits for the DETACHED launch tail to drive the run to `want` —
// the POST answers before dispatch (P5), so the state a second launch races is
// whatever finishHarnessLoginLaunch has reached by then.
func waitRunState(t *testing.T, st *supersedeStore, id string, want types.RunState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st.stateOf(t, id) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("run %s is %s after 5s, want %s", id, st.stateOf(t, id), want)
}

// TestHarnessLogin_NewLaunchSupersedesTheCallersLiveLoginRun is the finding
// itself: two sign-ins by ONE person leave ONE live sandbox — the newest.
//
// Red on the unfixed tree: both stay live, and the old one's unattended capture
// is then free to overwrite the new one's.
func TestHarnessLogin_NewLaunchSupersedesTheCallersLiveLoginRun(t *testing.T) {
	srv, _, st := supersedeLoginSrv(t, nil, nil)
	sess := memberLoginSession(t)

	first := launchLoginRun(t, srv, sess)
	waitRunState(t, st, first, types.RunRunning)

	second := launchLoginRun(t, srv, sess)
	if got := st.stateOf(t, first); got != types.RunKilled {
		t.Fatalf("the abandoned sign-in is %s, want KILLED — a second live login sandbox is finding 7: "+
			"its unattended capture lands after the new one's and the pane refuses a credential that works", got)
	}
	if got := st.stateOf(t, second); got.IsTerminal() {
		t.Fatalf("the NEW sign-in is %s — the supersede killed the run it exists to protect", got)
	}
}

// TestHarnessLogin_SupersedePrecedesTheQuota: the supersede runs BEFORE
// newStepRun, so the slot the orphan held is free by the time the quota is
// counted. A member capped at one concurrent run must be able to retry their own
// sign-in — otherwise the fix for finding 7 hands them a 403 instead.
func TestHarnessLogin_SupersedePrecedesTheQuota(t *testing.T) {
	srv, _, st := supersedeLoginSrv(t, assignedStore(limitsProfile("one-at-a-time",
		types.GovernanceLimits{MaxConcurrentRuns: 1})), nil)
	// A GROUP-carrying session: the profile above is assigned by group tier, and
	// a session with no groups snapshot is refused before the quota is ever read.
	sess := govSession(t, "sub-capped", []string{"eng"}, false)

	first := launchLoginRun(t, srv, sess)
	waitRunState(t, st, first, types.RunRunning)

	// launchLoginRun fails the test on anything but a 200 — under the quota the
	// unfixed tree answers 403 "too many runs at once".
	second := launchLoginRun(t, srv, sess)
	if got := st.stateOf(t, first); got != types.RunKilled {
		t.Errorf("first run = %s, want KILLED", got)
	}
	if got := st.stateOf(t, second); got.IsTerminal() {
		t.Errorf("second run = %s, want a live sandbox", got)
	}
}

// TestHarnessLogin_NeverKillsAnotherPersonsLoginRun is the negative control that
// matters most: the supersede is scoped to the CALLER's own runs. A shared
// deployment where one person's sign-in ends another's would be a denial of
// service with an audit row saying Wardyn did it on purpose.
func TestHarnessLogin_NeverKillsAnotherPersonsLoginRun(t *testing.T) {
	srv, _, st := supersedeLoginSrv(t, nil, nil)

	theirs := st.seed(types.AgentRun{
		ID: uuid.New(), CreatedBy: "sub-other", Task: harnessLoginTask, Agent: awsSSOAgent,
	})
	launchLoginRun(t, srv, memberLoginSession(t))

	if got := st.stateOf(t, theirs.ID.String()); got != types.RunRunning {
		t.Fatalf("another person's sign-in sandbox is %s, want RUNNING — the supersede is the caller's own runs only", got)
	}
}

// TestHarnessLogin_NeverKillsANonLoginRun: same person, but an ordinary run of
// theirs (and a login run on a DIFFERENT agent — the subscription login box).
// Neither is the sandbox a new AWS sign-in supersedes, and killing a member's
// working run because they signed in would be the worst possible reading of
// "one live sign-in per person".
func TestHarnessLogin_NeverKillsANonLoginRun(t *testing.T) {
	srv, _, st := supersedeLoginSrv(t, nil, nil)
	sess := memberLoginSession(t)

	// The caller's own principal, taken from a real launch so the test cannot
	// pass by guessing the wrong created_by.
	first := launchLoginRun(t, srv, sess)
	waitRunState(t, st, first, types.RunRunning)
	st.mu.Lock()
	actor := st.runs[uuid.MustParse(first)].CreatedBy
	st.mu.Unlock()

	work := st.seed(types.AgentRun{
		ID: uuid.New(), CreatedBy: actor, Task: "ship the thing", Agent: awsSSOAgent,
	})
	otherLane := st.seed(types.AgentRun{
		ID: uuid.New(), CreatedBy: actor, Task: harnessLoginTask, Agent: "claude-code",
	})

	launchLoginRun(t, srv, sess)

	if got := st.stateOf(t, work.ID.String()); got != types.RunRunning {
		t.Errorf("the caller's ordinary run is %s, want RUNNING — a sign-in must never end their work", got)
	}
	if got := st.stateOf(t, otherLane.ID.String()); got != types.RunRunning {
		t.Errorf("the caller's OTHER login lane is %s, want RUNNING — the supersede is per agent", got)
	}
}

// TestHarnessLogin_SupersedeAuditsSuccessWithTheReason: the supersede's kill is
// a SUCCESSFUL kill carrying a reason, not a failed one.
//
// The trap this pins: handleKillRun computes outcome=failure from a non-empty
// per-step error map, so carrying `superseded_by_new_login` IN that map would
// audit every supersede as a failure, add a bogus run.revoke/failure row, and —
// on the handler path — take the 500 branch. The reason rides the row's DATA
// beside the errors, never inside them.
func TestHarnessLogin_SupersedeAuditsSuccessWithTheReason(t *testing.T) {
	srv, audit, st := supersedeLoginSrv(t, nil, nil)
	sess := memberLoginSession(t)

	first := launchLoginRun(t, srv, sess)
	waitRunState(t, st, first, types.RunRunning)
	launchLoginRun(t, srv, sess)

	var row *types.AuditEvent
	for _, ev := range audit.find("run.kill") {
		if ev.Target == first {
			row = &ev
			break
		}
	}
	if row == nil {
		t.Fatal("no run.kill row for the superseded sign-in — the kill must be in the trail, not just in the store")
	}
	if row.Outcome != "success" {
		t.Errorf("run.kill outcome = %q, want success — a clean supersede is not a failed kill", row.Outcome)
	}
	var data map[string]any
	if err := json.Unmarshal(row.Data, &data); err != nil {
		t.Fatalf("decode run.kill data: %v", err)
	}
	if data["reason"] != supersedeReasonNewLogin {
		t.Errorf("run.kill reason = %v, want %q", data["reason"], supersedeReasonNewLogin)
	}
	for _, ev := range audit.find("run.revoke") {
		if ev.Target == first {
			t.Errorf("a run.revoke/%s row was written for a supersede whose cascade succeeded", ev.Outcome)
		}
	}
}

// TestUploadSSOToken_KilledRunIsRefused is the belt under the supersede.
//
// Killing a run revokes its identity, so arm 1 is the normal outcome: the token
// no longer verifies. But RevokeRun is best-effort and /internal/sso-token/ is
// one of the routes a TERMINAL run may still use for five minutes
// (internal_live_run.go), so revocation cannot be the ONLY guard — arm 2 is the
// same upload with a token that still verifies, and the run's own KILLED state
// has to refuse it. Without arm 2 a superseded sandbox's late capture overwrites
// the one the person just made.
func TestUploadSSOToken_KilledRunIsRefused(t *testing.T) {
	newKilledRunSrv := func(t *testing.T) (*harness, *Server, string, uuid.UUID) {
		t.Helper()
		h := newHarness(t)
		runID := uuid.New()
		st := ssoLoginRunStore{
			run: types.AgentRun{
				ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent, State: types.RunKilled,
				// INSIDE terminalUploadGrace (internal_live_run.go): the five
				// minutes a terminal run's own tail upload is still admitted is
				// precisely the window this guard exists for — a row stamped
				// long ago is refused by the middleware and never reaches the
				// handler, which would make this case pass for the wrong reason.
				UpdatedAt: time.Now().UTC(),
			},
			events: ssoLoginStartedEvents(runID, "https://my-sso.awsapps.com/start"),
		}
		cfg := baseTestConfig(h, st)
		cfg.Secrets = &memSecrets{m: map[string][]byte{}}
		cfg.BedrockRegion = "us-west-2"
		srv := New(cfg)
		return h, srv, h.mintRunToken(t, runID), runID
	}

	t.Run("the kill revoked the token", func(t *testing.T) {
		h, srv, tok, runID := newKilledRunSrv(t)
		if err := h.idp.RevokeRun(context.Background(), runID); err != nil {
			t.Fatalf("revoke run identity: %v", err)
		}
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401 — a revoked run token must not authenticate; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("the revoke failed and the run state is the guard", func(t *testing.T) {
		h, srv, tok, runID := newKilledRunSrv(t)
		// No RevokeRun at all: this IS the best-effort revoke having failed.
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
		if w.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409 — a KILLED login run's late upload must be refused by the run state; body=%s",
				w.Code, w.Body.String())
		}
		var refused *types.AuditEvent
		for _, ev := range h.audit.events {
			if ev.Action == "harness.credential.refused" {
				refused = &ev
				break
			}
		}
		if refused == nil {
			t.Fatal("no harness.credential.refused row — a refused capture that leaves no trail is the 0.7.3 finding again")
		}
		var data map[string]any
		if err := json.Unmarshal(refused.Data, &data); err != nil {
			t.Fatalf("decode refusal data: %v", err)
		}
		if data["reason"] != refuseReasonRunKilled {
			t.Errorf("refusal reason = %v, want %q", data["reason"], refuseReasonRunKilled)
		}
	})
}
