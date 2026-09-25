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

// SetSandboxRef overrides govEscapeStore's no-op so a gated-runner test can
// observe killTeardownTail actually reach KillSandbox: the embedded store's
// own SetSandboxRef discards the ref, which would leave every run's
// SandboxRef "" and skip the teardown's KillSandbox call outright.
func (s *supersedeStore) SetSandboxRef(_ context.Context, id uuid.UUID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil
	}
	r.SandboxRef = ref
	s.runs[id] = r
	return nil
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

// waitForRevoke polls for runID's identity revocation, so a test can assert on
// a supersede's teardown tail, which now runs on a detached goroutine after
// the launch POST answers (#122).
func waitForRevoke(t *testing.T, idp *ctxAwareIdentity, runID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if idp.didRevoke(runID) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("run %s identity was never revoked after 5s", runID)
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
	return ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
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

// TestMemberPreview_SignInRefusalPrecedesTheSupersede pins the ORDER of the two
// guards on this route, which nothing else does.
//
// The no-credential preview's 409 sits in handleHarnessLogin, before
// launchHarnessLoginRun and therefore before the supersede; the other preview
// case asserts only "no run row, no harness.login.started" on a fixture with no
// supersede seam and no live login run, so moving the 409 below the launch (or
// hoisting the supersede into the handler — a plausible refactor, since the
// comment at the supersede call already argues about placement) would kill the
// admin's real sign-in from inside a preview with every other test still green.
// That is the worst possible shape of this feature: a view that destroys the
// thing it is pretending not to have.
func TestMemberPreview_SignInRefusalPrecedesTheSupersede(t *testing.T) {
	f := newSupersedeFixture(t, nil, nil)

	// The admin's own live sign-in — the run a supersede reached from inside the
	// preview would end. Its created_by is the preview session's subject, so it
	// IS selected by supersedeCallerLoginRuns' creator+task+agent key.
	live := f.store.seed(types.AgentRun{
		ID: uuid.New(), CreatedBy: memberPreviewAdminSub, Task: harnessLoginTask, Agent: awsSSOAgent,
	})

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/setup/harness-login",
		memberPreviewSession(t, true, true), `{"provider":"`+awsSSOProvider+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("harness-login in the preview = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), memberPreviewSignInRefusal) {
		t.Errorf("body = %q, want %q", w.Body.String(), memberPreviewSignInRefusal)
	}
	if got := f.store.stateOf(t, live.ID.String()); got != types.RunRunning {
		t.Errorf("the admin's own sign-in sandbox is %s, want RUNNING — a refused launch must not "+
			"supersede anything: the preview hides their credential, it must not destroy their session", got)
	}
	if rows := f.audit.find("run.kill"); len(rows) != 0 {
		t.Errorf("a refused sign-in wrote %d run.kill row(s); the supersede ran below the refusal", len(rows))
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

// TestHarnessLogin_APassSeeingBothConcurrentRunsLeavesOne is the race the first
// pass cannot win on its own.
//
// supersede-then-create is not atomic: each launch reads the live runs BEFORE
// its own row exists, so a double-click (or the console and a `wdn_` token on
// one subject) leaves two live sign-in sandboxes — and the field report's defect
// with them, since the loser completes its login unattended and its late capture
// lands after the winner's. The second pass is what closes it, and it has to do
// so WITHOUT an in-process mutex, which is not a lock on the second replica.
//
// What is pinned, exactly: the interleaving where a pass sees both rows — both
// are in the store before either second pass runs. That is the ordinary
// double-click, and the passes are run in each order because the property is
// that the answer does not depend on which finishes first. It is NOT a universal
// claim: created_at is stamped in-process before the insert, so a launch whose
// clock runs behind can insert AFTER a sibling's pass has already run and leave
// two survivors (see supersedeOlderLoginRuns' own note, and the 0.7.6 follow-up).
func TestHarnessLogin_APassSeeingBothConcurrentRunsLeavesOne(t *testing.T) {
	const actor = "sub-double-click"
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	setup := func(t *testing.T) (supersedeFixture, types.AgentRun, types.AgentRun, types.AgentRun) {
		t.Helper()
		f := newSupersedeFixture(t, nil, nil)
		older := f.store.seed(types.AgentRun{
			ID: uuid.New(), CreatedBy: actor, Task: harnessLoginTask, Agent: awsSSOAgent, CreatedAt: base,
		})
		newer := f.store.seed(types.AgentRun{
			ID: uuid.New(), CreatedBy: actor, Task: harnessLoginTask, Agent: awsSSOAgent,
			CreatedAt: base.Add(time.Millisecond),
		})
		// The negative control travels with the race: a second person's sign-in is
		// never in this order at all.
		theirs := f.store.seed(types.AgentRun{
			ID: uuid.New(), CreatedBy: "sub-other", Task: harnessLoginTask, Agent: awsSSOAgent, CreatedAt: base,
		})
		return f, older, newer, theirs
	}

	check := func(t *testing.T, f supersedeFixture, older, newer, theirs types.AgentRun) {
		t.Helper()
		if got := f.store.stateOf(t, older.ID.String()); got != types.RunKilled {
			t.Errorf("the older concurrent sign-in is %s, want KILLED — two live sign-in sandboxes for one "+
				"person is finding 7: the loser's unattended capture overwrites the winner's", got)
		}
		if got := f.store.stateOf(t, newer.ID.String()); got != types.RunRunning {
			t.Errorf("the newer concurrent sign-in is %s, want RUNNING — a tie-break that leaves ZERO live "+
				"sign-ins is worse than the defect: the person is told to sign in again, forever", got)
		}
		if got := f.store.stateOf(t, theirs.ID.String()); got != types.RunRunning {
			t.Errorf("another person's sign-in sandbox is %s, want RUNNING", got)
		}
	}

	t.Run("the newer launch finishes its pass first", func(t *testing.T) {
		f, older, newer, theirs := setup(t)
		f.srv.supersedeOlderLoginRuns(context.Background(), actor, awsSSOAgent, newer)
		f.srv.supersedeOlderLoginRuns(context.Background(), actor, awsSSOAgent, older)
		check(t, f, older, newer, theirs)
	})

	t.Run("the older launch finishes its pass first", func(t *testing.T) {
		f, older, newer, theirs := setup(t)
		f.srv.supersedeOlderLoginRuns(context.Background(), actor, awsSSOAgent, older)
		f.srv.supersedeOlderLoginRuns(context.Background(), actor, awsSSOAgent, newer)
		check(t, f, older, newer, theirs)
	})

	// Same created_at to the nanosecond — a coarse clock, or two rows stamped in
	// the same tick — again with both rows visible to both passes. The id
	// tie-break still has to name ONE survivor: an order that is not total
	// degenerates to "neither supersedes the other", which is the defect, or to
	// "each supersedes the other", which is zero.
	t.Run("an exactly equal clock reading still leaves one", func(t *testing.T) {
		f := newSupersedeFixture(t, nil, nil)
		a := f.store.seed(types.AgentRun{
			ID: uuid.New(), CreatedBy: actor, Task: harnessLoginTask, Agent: awsSSOAgent, CreatedAt: base,
		})
		b := f.store.seed(types.AgentRun{
			ID: uuid.New(), CreatedBy: actor, Task: harnessLoginTask, Agent: awsSSOAgent, CreatedAt: base,
		})
		f.srv.supersedeOlderLoginRuns(context.Background(), actor, awsSSOAgent, a)
		f.srv.supersedeOlderLoginRuns(context.Background(), actor, awsSSOAgent, b)
		live := 0
		for _, run := range []types.AgentRun{a, b} {
			if !f.store.stateOf(t, run.ID.String()).IsTerminal() {
				live++
			}
		}
		if live != 1 {
			t.Fatalf("%d live sign-in sandboxes after two passes, want exactly 1", live)
		}
	})
}

// TestHarnessLogin_TheSecondPassSparesTheRunItJustMade: the HTTP launch drives
// both passes, and the second one must not end the run the caller is about to be
// handed. The pass is keyed on a total order over the caller's OWN live runs, so
// a launch with nothing else in flight has to be a no-op.
func TestHarnessLogin_TheSecondPassSparesTheRunItJustMade(t *testing.T) {
	f := newSupersedeFixture(t, nil, nil)
	run := launchLoginRun(t, f.srv, memberLoginSession(t))
	waitRunState(t, f.store, run, types.RunRunning)
	if got := f.store.stateOf(t, run); got.IsTerminal() {
		t.Fatalf("the only sign-in is %s — the second pass superseded the run it just created", got)
	}
	if rows := f.audit.find("run.kill"); len(rows) != 0 {
		t.Errorf("a lone launch wrote %d run.kill row(s), want none", len(rows))
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

	// The run.kill row is written by the detached teardown tail (#122), which
	// runs after this POST has already answered — wait for it rather than
	// asserting immediately.
	row := waitForKillRow(t, audit, first)
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

// toctouRunStore is the login-run store with a switch: the run reads RUNNING
// until kill() is called, KILLED after. It models the supersede, which takes
// none of the upload route's locks and can therefore land at any instant.
type toctouRunStore struct {
	ssoLoginRunStore
	mu     sync.Mutex
	killed bool
}

func (s *toctouRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.ssoLoginRunStore.run
	if s.killed {
		run.State = types.RunKilled
	}
	return run, nil
}

func (s *toctouRunStore) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killed = true
}

// killOnReadSecrets fires that switch from INSIDE the critical section: the
// once-only guard's read is taken under lockAWSSSOOwner, after the handler's
// top-of-route KILLED check has already passed. Hooking the kill here is what
// puts it in the window rather than before or after it.
type killOnReadSecrets struct {
	*memSecrets
	runs *toctouRunStore
}

func (s *killOnReadSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	s.runs.kill()
	return s.memSecrets.Get(ctx, name)
}

// TestUploadSSOToken_KilledInsideTheLockIsRefused is the TOCTOU under the belt.
//
// The handler reads the run state ONCE at the top and then does a great deal
// before it writes: the body, the launch stamp, the bindings, the scope, the
// lock wait, the once-only read. A supersede landing anywhere in there — the
// person's own next sign-in, which is the commonest thing to happen while a
// login sandbox is still polling — leaves this upload storing AFTER the new
// sandbox's, which is the late-capture ordering the whole supersede exists to
// prevent, arriving through a door that had already been checked.
//
// The guard is the state re-read as the last statement before the write. This
// case fails on the unfixed tree with a 204 and a stored blob.
func TestUploadSSOToken_KilledInsideTheLockIsRefused(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &toctouRunStore{ssoLoginRunStore: ssoLoginRunStore{
		run: types.AgentRun{
			ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent, State: types.RunRunning,
			UpdatedAt: time.Now().UTC(),
		},
		events: ssoLoginStartedEvents(runID, "https://my-sso.awsapps.com/start"),
	}}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = &killOnReadSecrets{memSecrets: sec, runs: st}
	cfg.BedrockRegion = "us-west-2"
	srv := New(cfg)
	h.srv = srv
	tok := h.mintRunToken(t, runID)

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 — a run killed while this upload was in flight must not store; body=%s",
			w.Code, w.Body.String())
	}
	if _, stored := sec.m[harnessCredSecretName(awsSSOProvider)]; stored {
		t.Error("the superseded sandbox's capture was stored anyway — it is now the credential the person's " +
			"NEW sign-in will be told it cannot confirm")
	}
	var refused *types.AuditEvent
	for _, ev := range h.audit.events {
		if ev.Action == "harness.credential.refused" {
			refused = &ev
			break
		}
	}
	if refused == nil {
		t.Fatal("no harness.credential.refused row for a refused capture")
	}
	data := killData(t, *refused)
	if data["reason"] != refuseReasonRunKilled {
		t.Errorf("refusal reason = %v, want %q — the same reason the top-of-route check answers, "+
			"so the two arms group as one thing in an incident review", data["reason"], refuseReasonRunKilled)
	}
	// The scope IS decided by the time this arm fires, so the row carries the
	// pair that makes a per_user estate's refusal stream groupable by person.
	if _, ok := data["owner"]; !ok {
		t.Errorf("refusal data = %v, want the owner/credential_source pair beside the reason", data)
	}
}

// TestKillRunCascade_FinishesAfterTheCallersContextDies: the cascade
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
// that tab mid-launch is this lane's whole subject), and the kill route
// carries the same detach.
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

		// The CAS (claimKillTransition) is synchronous, so the state change is
		// already visible; the revoke and the run.kill row are the detached
		// teardown tail's (#122) and have to be waited for.
		if got := f.store.stateOf(t, first); got != types.RunKilled {
			t.Fatalf("run state = %s, want KILLED", got)
		}
		waitForRevoke(t, f.idp, uuid.MustParse(first))
		waitForKillRow(t, f.audit, first)
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
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
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

// waitForKillRow polls for the run.kill row belonging to runID, so a test can
// assert on a supersede's teardown tail — which now runs on a detached
// goroutine after the launch POST answers (#122), unlike handleKillRun's own
// synchronous cascade, which killRow still suits.
func waitForKillRow(t *testing.T, audit *memAudit, runID string) types.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range audit.find("run.kill") {
			if ev.Target == runID {
				return ev
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no run.kill row for run %s after 5s", runID)
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
		// The run.kill row is the detached teardown tail's (#122); wait for it.
		row := waitForKillRow(t, f.audit, first)
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

// TestHarnessLogin_UnenforceableClassRefusalPrecedesTheSupersede is the second
// ordering pin on this route, and the one 0.7.8 made necessary.
//
// The sign-in capture now dispatches at the strongest advertised class AT OR
// ABOVE the admin floor, and strongestAdvertisedAtOrAbove falls back to the
// floor itself when nothing advertised reaches it. So a CC1-only host under an
// admin floor of CC2 produces a class this runner cannot enforce — correctly, a
// refusal — but the refusal has to land BEFORE supersedeCallerLoginRuns, or the
// person's existing sign-in sandbox is killed to make room for a run that then
// fails in the driver. Refusing after destroying is the one order this route
// must never take.
func TestHarnessLogin_UnenforceableClassRefusalPrecedesTheSupersede(t *testing.T) {
	f := newSupersedeFixture(t, nil, &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1}})
	f.srv.cfg.DefaultPolicy.MinConfinementClass = types.CC2

	live := f.store.seed(types.AgentRun{
		ID: uuid.New(), CreatedBy: "sub-member", Task: harnessLoginTask, Agent: awsSSOAgent,
	})

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/setup/harness-login",
		memberLoginSession(t), `{"provider":"`+awsSSOProvider+`"}`)
	if w.Code == http.StatusOK || w.Code == http.StatusCreated {
		t.Fatalf("harness-login = %d on a CC1-only host under a CC2 floor, want a refusal: %s", w.Code, w.Body.String())
	}
	if got := f.store.stateOf(t, live.ID.String()); got != types.RunRunning {
		t.Errorf("the caller's own sign-in sandbox is %s, want RUNNING — the refusal ran BELOW the "+
			"supersede, so a person whose host cannot meet the floor loses the session they had", got)
	}
	if rows := f.audit.find("run.kill"); len(rows) != 0 {
		t.Errorf("a refused sign-in wrote %d run.kill row(s); the supersede ran below the refusal", len(rows))
	}
}

// gatedKillRunner blocks KillSandbox until the test closes the gate, so a test
// can observe exactly what a sign-in POST has and has not done by the time it
// answers — before the detached teardown tail has torn anything down — and
// what only happens once the gate opens. entered is closed the first time
// KillSandbox is actually reached, the only way a test can know the detached
// goroutine is blocked INSIDE the call rather than simply not scheduled yet
// (mirrors coldPullRunner.entered in harnesscred_launch_test.go).
type gatedKillRunner struct {
	*fakeRunner
	gate        chan struct{}
	gateOnce    sync.Once
	entered     chan struct{}
	enteredOnce sync.Once
}

func (g *gatedKillRunner) KillSandbox(ctx context.Context, ref string) error {
	g.enteredOnce.Do(func() { close(g.entered) })
	<-g.gate
	return g.fakeRunner.KillSandbox(ctx, ref)
}

// openGate opens the gate, idempotently — safe to call both from the test body
// (to observe what happens once KillSandbox unblocks) and from a t.Cleanup (so
// a case that fails BEFORE opening it does not leak the detached goroutine
// parked on <-g.gate for the rest of the test binary's run).
func (g *gatedKillRunner) openGate() {
	g.gateOnce.Do(func() { close(g.gate) })
}

// TestHarnessLogin_SignInAnswersBeforeTheSupersededTeardown is #122 itself: with
// KillSandbox gated shut, the sign-in POST answers — and the superseded run
// already reads KILLED — before the gate ever opens, and the run.kill row plus
// the identity revocation (the rest of killTeardownTail) happen only after it
// does. Undo the claim/tail split and this hangs until its own 2s deadline
// instead of passing in milliseconds.
func TestHarnessLogin_SignInAnswersBeforeTheSupersededTeardown(t *testing.T) {
	rnr := &gatedKillRunner{fakeRunner: &fakeRunner{}, gate: make(chan struct{}), entered: make(chan struct{})}
	t.Cleanup(rnr.openGate)
	f := newSupersedeFixture(t, nil, rnr)
	srv, st, audit, idp := f.srv, f.store, f.audit, f.idp
	sess := memberLoginSession(t)

	first := launchLoginRun(t, srv, sess)
	waitRunState(t, st, first, types.RunRunning)

	// The second POST, run on its own goroutine and answered through a channel
	// rather than asserted there — t.FailNow (which t.Fatal calls) may only run
	// on the test's own goroutine — so a synchronous teardown fails
	// this test with a clear timeout instead of hanging until `go test`'s own
	// deadline.
	type postResult struct {
		code int
		body string
	}
	resultCh := make(chan postResult, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/setup/harness-login", strings.NewReader(`{"provider":"aws"}`))
		r.AddCookie(sess)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		resultCh <- postResult{code: w.Code, body: w.Body.String()}
	}()

	var second string
	select {
	case res := <-resultCh:
		if res.code != http.StatusOK {
			t.Fatalf("POST /setup/harness-login = %d, want 200: %s", res.code, res.body)
		}
		var body struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal([]byte(res.body), &body); err != nil {
			t.Fatalf("decode launch response: %v (%s)", err, res.body)
		}
		second = body.RunID
	case <-time.After(2 * time.Second):
		t.Fatal("the sign-in POST did not answer within 2s while the superseded run's teardown was gated " +
			"shut — it must not wait on KillSandbox")
	}
	if second == "" {
		t.Fatal("launch answered no run id")
	}

	// Answered already, so the claim — the synchronous half — is done: the
	// superseded run reads KILLED before the gate ever opens.
	if got := st.stateOf(t, first); got != types.RunKilled {
		t.Fatalf("superseded run state = %s, want KILLED before the teardown gate opens", got)
	}

	select {
	case <-rnr.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the detached teardown never reached KillSandbox")
	}

	// The gate is still shut: nothing past the claim has run yet.
	if rows := audit.find("run.kill"); len(rows) != 0 {
		t.Fatalf("run.kill row(s) written before the teardown gate opened: %d", len(rows))
	}
	if idp.didRevoke(uuid.MustParse(first)) {
		t.Fatal("the run's identity was revoked before the teardown gate opened")
	}

	rnr.openGate()

	// Only now — after the gate opens — does the rest of the tail land.
	waitForKillRow(t, audit, first)
	waitForRevoke(t, idp, uuid.MustParse(first))
}

// TestServer_WaitBackground_WaitsForKillTeardownTail is the shutdown half of
// #122's review: httpSrv.Shutdown (cmd/wardynd/boot_serve.go) only waits for
// in-flight HANDLERS, and the supersede's teardown is a goroutine that
// deliberately outlives its handler. Without goBackground/WaitBackground, a
// SIGTERM landing between the claim and the teardown would leave the
// superseded run KILLED with its sandbox still up, its credentials unrevoked
// and no run.kill row — the shutdown answers before the tail does. This pins
// that WaitBackground actually blocks until the tail (run through goBackground
// by supersedeOneLoginRun) finishes, and that the run.kill row is ALREADY
// written by the moment it returns — not merely "eventually", which is what a
// caller doing an orderly stop needs to be true.
func TestServer_WaitBackground_WaitsForKillTeardownTail(t *testing.T) {
	rnr := &gatedKillRunner{fakeRunner: &fakeRunner{}, gate: make(chan struct{}), entered: make(chan struct{})}
	t.Cleanup(rnr.openGate)
	f := newSupersedeFixture(t, nil, rnr)
	srv, st, audit := f.srv, f.store, f.audit
	sess := memberLoginSession(t)

	first := launchLoginRun(t, srv, sess)
	waitRunState(t, st, first, types.RunRunning)
	launchLoginRun(t, srv, sess) // supersedes `first`; the tail blocks on rnr.gate

	select {
	case <-rnr.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the detached teardown never reached KillSandbox")
	}

	// WaitBackground, run on its own goroutine so the test can tell "still
	// blocked" from "returned" without WaitBackground itself hanging the test.
	waitDone := make(chan struct{})
	go func() {
		srv.WaitBackground()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("WaitBackground returned while the teardown was still gated shut — shutdown must wait for it")
	case <-time.After(100 * time.Millisecond):
	}

	rnr.openGate()

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitBackground never returned after the gate opened")
	}

	// The run.kill row must already be there — WaitBackground returning is the
	// caller's signal that it is safe to close the audit sinks next.
	if rows := audit.find("run.kill"); len(rows) == 0 {
		t.Error("WaitBackground returned with no run.kill row written — the shutdown path would have raced the teardown")
	}
}

// TestServer_WaitBackground_RespectsItsBudget is the other half: a detached
// goroutine that never finishes (a genuinely wedged runner call) must not turn
// an orderly shutdown into a hang. bgWaitBudget overrides the real ~35s budget
// so this proves the bound in milliseconds.
func TestServer_WaitBackground_RespectsItsBudget(t *testing.T) {
	srv := New(baseTestConfig(newHarness(t), &integStore{govEscapeStore: newGovEscapeStore(&capStore{})}))
	srv.bgWaitBudget = 50 * time.Millisecond

	never := make(chan struct{})
	t.Cleanup(func() { close(never) }) // let the leaked goroutine finish once the test is done
	srv.goBackground(func() { <-never })

	start := time.Now()
	srv.WaitBackground()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("WaitBackground took %s with a 50ms budget and a goroutine that never finishes — it did not respect its bound", elapsed)
	}
}
