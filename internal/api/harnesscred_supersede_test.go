// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
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

// ctxAwareIdentity is the harness identity plus the two things the cascade's
// contract needs a test to see: WHETHER RevokeRun ran, and whether it ran on a
// LIVE context. The ctx check is what makes R1-F1 testable at all — a kill that
// leaked the caller's cancellation would reach RevokeRun with a dead context,
// which in production (a store write) fails exactly like this.
type ctxAwareIdentity struct {
	identity.Provider
	mu        sync.Mutex
	revoked   []uuid.UUID
	revokeErr error
}

func (i *ctxAwareIdentity) RevokeRun(ctx context.Context, runID uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	failing := i.revokeErr
	if failing == nil {
		i.revoked = append(i.revoked, runID)
	}
	i.mu.Unlock()
	if failing != nil {
		return failing
	}
	return i.Provider.RevokeRun(ctx, runID)
}

func (i *ctxAwareIdentity) didRevoke(runID uuid.UUID) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, id := range i.revoked {
		if id == runID {
			return true
		}
	}
	return false
}

func (i *ctxAwareIdentity) failRevokes(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.revokeErr = err
}

// ctxAwareAudit drops a row written on a dead context, which is what the real
// sink does — recordAudit passes the context straight to a store write. Without
// it a cancelled cascade still "audits" in these tests and R1-F1 is unprovable.
type ctxAwareAudit struct{ *memAudit }

func (a *ctxAwareAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.memAudit.Record(ctx, ev)
}

// supersedeFixture is one login deployment: the roster, the store that answers
// the supersede's two reads, an identity whose revocations are observable, and
// a context-honest audit sink.
type supersedeFixture struct {
	srv   *Server
	audit *memAudit
	store *supersedeStore
	idp   *ctxAwareIdentity
}

func newSupersedeFixture(t *testing.T, cs *capStore, rnr runner.Runner) supersedeFixture {
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
	idp := &ctxAwareIdentity{Provider: h.idp}
	cfg := baseTestConfig(h, st)
	cfg.Identity = idp
	cfg.Audit = &ctxAwareAudit{memAudit: audit}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = rnr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	return supersedeFixture{srv: New(cfg), audit: audit, store: st, idp: idp}
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
	f := newSupersedeFixture(t, nil, nil)
	srv, st := f.srv, f.store
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
	f := newSupersedeFixture(t, assignedStore(limitsProfile("one-at-a-time",
		types.GovernanceLimits{MaxConcurrentRuns: 1})), nil)
	srv, st := f.srv, f.store
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
	f := newSupersedeFixture(t, nil, nil)
	srv, st := f.srv, f.store

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
	f := newSupersedeFixture(t, nil, nil)
	srv, st := f.srv, f.store
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
	f := newSupersedeFixture(t, nil, nil)
	srv, audit, st := f.srv, f.audit, f.store
	sess := memberLoginSession(t)

	first := launchLoginRun(t, srv, sess)
	waitRunState(t, st, first, types.RunRunning)
	second := launchLoginRun(t, srv, sess)

	row := killRow(t, audit, first)
	if row.Outcome != "success" {
		t.Errorf("run.kill outcome = %q, want success — a clean supersede is not a failed kill", row.Outcome)
	}
	// docs/AUDIT-ACTIONS.md promises the reader all four of these; a row that
	// says only "killed" sends whoever lost their sandbox back to the code.
	if row.ActorType != types.ActorSystem || row.Actor != "wardynd" {
		t.Errorf("run.kill actor = %s/%s, want system/wardynd — nobody asked for this kill", row.ActorType, row.Actor)
	}
	data := killData(t, row)
	if data["reason"] != supersedeReasonNewLogin {
		t.Errorf("run.kill reason = %v, want %q", data["reason"], supersedeReasonNewLogin)
	}
	st.mu.Lock()
	actor := st.runs[uuid.MustParse(first)].CreatedBy
	st.mu.Unlock()
	if data["superseded_for"] != actor {
		t.Errorf("run.kill superseded_for = %v, want %q", data["superseded_for"], actor)
	}
	if data["superseded_by_run"] != second {
		t.Errorf("run.kill superseded_by_run = %v, want the new run %q", data["superseded_by_run"], second)
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

// TestKillRunCascade_FinishesAfterTheCallersContextDies is R1-F1: the cascade
// detaches ITSELF, so no caller can leak a cancellation into a half-applied
// kill.
//
// The failure it pins is specific. Past the KILLED CAS, a dead context cancels
// KillSandbox, makes retryQuick bail before revoking, and fails the run.kill
// write — leaving a row that says KILLED, a sandbox that is alive, an identity
// that still mints, and NOTHING in the trail. Nothing revisits that state: the
// idle reaper lists RUNNING and the next supersede selects non-terminal runs.
//
// BOTH callers, because the property belongs to the cascade and not to either
// of them: the supersede runs on the launch POST's context (a person closing
// that tab mid-launch is this lane's whole subject), and the kill route is the
// regression pin for the detach it has always had.
func TestKillRunCascade_FinishesAfterTheCallersContextDies(t *testing.T) {
	t.Run("the login supersede", func(t *testing.T) {
		f := newSupersedeFixture(t, nil, nil)
		sess := memberLoginSession(t)
		first := launchLoginRun(t, f.srv, sess)
		waitRunState(t, f.store, first, types.RunRunning)

		f.store.mu.Lock()
		actor := f.store.runs[uuid.MustParse(first)].CreatedBy
		f.store.mu.Unlock()

		// The caller's context, already gone — a closed tab, mid-launch.
		dead, cancel := context.WithCancel(context.Background())
		cancel()
		f.srv.supersedeCallerLoginRuns(dead, actor, awsSSOAgent, uuid.New())

		if got := f.store.stateOf(t, first); got != types.RunKilled {
			t.Fatalf("run state = %s, want KILLED", got)
		}
		if !f.idp.didRevoke(uuid.MustParse(first)) {
			t.Error("the run's identity was never revoked — the cascade inherited the caller's dead context, " +
				"so the sandbox keeps a token that still mints")
		}
		if !supersedeKillRow(t, f.audit, first) {
			t.Error("no run.kill row — a terminal state change with nothing in the trail, and nothing revisits it")
		}
	})

	t.Run("the kill route", func(t *testing.T) {
		f := newSupersedeFixture(t, nil, nil)
		sess := memberLoginSession(t)
		run := launchLoginRun(t, f.srv, sess)
		waitRunState(t, f.store, run, types.RunRunning)

		dead, cancel := context.WithCancel(context.Background())
		cancel()
		w := doSSOCtx(t, f.srv, dead, http.MethodPost, "/api/v1/runs/"+run+"/kill", sess, "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("kill: code = %d, want 202; body=%s", w.Code, w.Body.String())
		}
		if !f.idp.didRevoke(uuid.MustParse(run)) {
			t.Error("the kill route left the identity unrevoked on a disconnected client")
		}
		if !supersedeKillRow(t, f.audit, run) {
			t.Error("the kill route wrote no run.kill row on a disconnected client")
		}
	})
}

// doSSOCtx is doSSO with a caller-supplied request context — a client whose
// connection is already gone by the time the handler runs.
func doSSOCtx(t *testing.T, srv *Server, ctx context.Context, method, path string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// supersedeKillRow reports whether a run.kill row exists for runID.
func supersedeKillRow(t *testing.T, audit *memAudit, runID string) bool {
	t.Helper()
	for _, ev := range audit.find("run.kill") {
		if ev.Target == runID {
			return true
		}
	}
	return false
}

// killRow returns the run.kill row for runID, failing the test if there is none.
func killRow(t *testing.T, audit *memAudit, runID string) types.AuditEvent {
	t.Helper()
	for _, ev := range audit.find("run.kill") {
		if ev.Target == runID {
			return ev
		}
	}
	t.Fatalf("no run.kill row for run %s", runID)
	return types.AuditEvent{}
}

// TestKillRunCascade_PartialCascadeIsHonest is R1-F2: the branch where a
// teardown step FAILS had no test in either caller, and this lane both moved it
// and gave it a second consumer.
//
// The two callers must disagree, deliberately. The kill ROUTE is a governance
// control answering a human: it reports 500 with a run.revoke row so the
// operator retries rather than believing the run is contained. The SUPERSEDE is
// a step inside somebody's sign-in: it proceeds — a person must not be locked
// out of signing in because the old run's revoke failed — and the honest record
// is the run.kill row carrying BOTH the supersede reason and the failing step.
func TestKillRunCascade_PartialCascadeIsHonest(t *testing.T) {
	revokeFailed := errors.New("revocation store unavailable")

	t.Run("the kill route reports 500 with a run.revoke row", func(t *testing.T) {
		f := newSupersedeFixture(t, nil, nil)
		sess := memberLoginSession(t)
		run := launchLoginRun(t, f.srv, sess)
		waitRunState(t, f.store, run, types.RunRunning)
		f.idp.failRevokes(revokeFailed)

		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+run+"/kill", sess, "")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500 — a kill whose revoke failed must not report success; body=%s",
				w.Code, w.Body.String())
		}
		row := killRow(t, f.audit, run)
		if row.Outcome != "failure" {
			t.Errorf("run.kill outcome = %q, want failure", row.Outcome)
		}
		if got := killData(t, row)["identity_error"]; got == nil {
			t.Errorf("run.kill data = %s, want an identity_error naming the failing step", row.Data)
		}
		revokes := 0
		for _, ev := range f.audit.find("run.revoke") {
			if ev.Target == run {
				revokes++
			}
		}
		if revokes != 1 {
			t.Errorf("run.revoke rows for this run = %d, want exactly 1", revokes)
		}
	})

	t.Run("the supersede proceeds and audits the failure", func(t *testing.T) {
		f := newSupersedeFixture(t, nil, nil)
		sess := memberLoginSession(t)
		first := launchLoginRun(t, f.srv, sess)
		waitRunState(t, f.store, first, types.RunRunning)
		f.idp.failRevokes(revokeFailed)

		// launchLoginRun fails the test on anything but 200: the new sign-in must
		// NOT be blocked by the old run's failed revoke. The capture PUT's KILLED
		// guard is what makes proceeding safe.
		second := launchLoginRun(t, f.srv, sess)
		if got := f.store.stateOf(t, second); got.IsTerminal() {
			t.Fatalf("the new sign-in is %s", got)
		}
		row := killRow(t, f.audit, first)
		if row.Outcome != "failure" {
			t.Errorf("run.kill outcome = %q, want failure — a supersede that could not revoke is not a clean kill", row.Outcome)
		}
		data := killData(t, row)
		if data["reason"] != supersedeReasonNewLogin {
			t.Errorf("run.kill reason = %v, want %q", data["reason"], supersedeReasonNewLogin)
		}
		if data["identity_error"] == nil {
			t.Errorf("run.kill data = %s, want the failing step beside the reason", row.Data)
		}
		// The 500-path row belongs to the HTTP kill, not to a supersede: nobody
		// asked for this kill, and a run.revoke/failure row here would read as an
		// operator's retryable incident.
		for _, ev := range f.audit.find("run.revoke") {
			if ev.Target == first {
				t.Errorf("a supersede wrote a run.revoke row (%s)", ev.Outcome)
			}
		}
	})
}

// killData decodes a run.kill row's DATA.
func killData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode run.kill data: %v (%s)", err, ev.Data)
	}
	return data
}
