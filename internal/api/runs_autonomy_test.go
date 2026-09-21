// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the autonomy fixture ─────────────────────────────────────────────────────

// autonomyRubric returns a rubric that resolves to level WHATEVER the posture
// is: all nine fields set to the same value.
//
// Deliberate. The fold's own arithmetic — which axis applies, which cap is the
// minimum, which field is named — is pinned in internal/composer
// (TestFoldAutonomyMinimum), and re-deriving it here would make every row of
// the ladder table below depend on the fixture's egress and grant shape
// instead of on the rung under test. What this table asks is the other
// question: given a level, what does the gate do to a request?
func autonomyRubric(level types.AutonomyLevel) *types.AutonomyRubric {
	return &types.AutonomyRubric{
		EgressOpen: level, EgressReviewed: level, EgressSealed: level,
		SecretsPowerful: level, SecretsBaseline: level, SecretsNone: level,
		ConfinementCC1: level, ConfinementCC2: level, ConfinementCC3: level,
	}
}

// autonomyProfile is the assigned profile the table runs under: govProfileSpec's
// ceiling, and a rubric as the ONLY limit.
//
// govProfile's own DenyInteractive is dropped on purpose. Every other
// GovernanceLimits field refuses a request shape this gate also refuses, so
// leaving one set would let a row pass for the wrong reason — the refusal it
// asserts would be the older gate's.
func autonomyProfile(level types.AutonomyLevel) *types.GovernanceProfile {
	p := govProfile("autonomy")
	p.Limits = types.GovernanceLimits{AutonomyRubric: autonomyRubric(level)}
	return p
}

func autonomyCapStore(p *types.GovernanceProfile) *capStore {
	return &capStore{govProfile: p, govTier: types.CapabilitySubjectGroup, govHasGroupTier: true}
}

// lastAuthzDenied returns the target of the last authz.denied row, or "" when
// there is none. The refusal target is what makes a 403 legible — which FIELD
// the caller has to change — so a table that asserted only the status could not
// tell the seed refusal from the exec one.
func lastAuthzDenied(events []types.AuditEvent) string {
	target := ""
	for i := range events {
		if events[i].Action == "authz.denied" {
			target = events[i].Target
		}
	}
	return target
}

// autonomyCreateAudit returns the run.create payload of the one run this
// fixture created. It fails the test when no run reached the store.
func autonomyCreateAudit(t *testing.T, st *govEscapeStore, audit *recRecorder) map[string]any {
	t.Helper()
	st.mu.Lock()
	var runID uuid.UUID
	for id := range st.runs {
		runID = id
	}
	st.mu.Unlock()
	ev := findAudit(audit.events, runID, "run.create", "success")
	if ev == nil {
		t.Fatalf("no run.create audit row for %s", runID)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("run.create payload is not an object: %v (%s)", err, ev.Data)
	}
	return data
}

// ─── the ladder table ─────────────────────────────────────────────────────────

// TestRunAutonomyLadder is the gate's behaviour table: four levels against the
// six request shapes the rungs are defined in terms of, driven end to end
// through POST /runs as an assigned member.
//
// Every cell is a decision that could have gone the other way, and two
// families of them are the reason the table is exhaustive rather than
// sampled:
//
//   - MONOTONICITY. L0 is the most supervised rung, so anything L1 refuses it
//     must refuse too. Nothing in the type system says so — Rank() is just an
//     int — and a gate written as one arm per rung ships that hole the first
//     time a case is added to L1 alone. `exec` at L0 is exactly that cell: the
//     issue's own per-rung wording lists it under L1 and L2 only.
//   - THE DERIVED HOLD. L1 permits an unattended run and refuses an
//     unsupervised one, so `auto` at L1 must come back `hold` — asserted on
//     the AUDIT ROW, which is the record dispatch's sandbox env is built from
//     the same request field as. An assertion on the response body would pass
//     while the run ran unsupervised.
func TestRunAutonomyLadder(t *testing.T) {
	const (
		holdRow = `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"hold"}`
		autoRow = `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`
	)
	type want struct {
		status        int
		target        string // authz.denied target, for a 403
		toolApprovals string // the audited tool_approvals, for a 201 ("" = the key is absent)
	}
	ok := func(ta string) want { return want{status: http.StatusCreated, toolApprovals: ta} }
	denied := func(target string) want { return want{status: http.StatusForbidden, target: target} }

	for _, shape := range []struct {
		name string
		body string
		// by level, L0 → L3
		want [4]want
	}{
		{
			name: "interactive",
			body: `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true}`,
			// Permitted at every rung, and NEVER derived to hold: an
			// interactive run refuses an explicit hold by design and dispatch
			// writes WARDYN_TOOL_APPROVALS for non-interactive runs alone, so a
			// derived one there is a field accepted and thrown away.
			want: [4]want{ok(""), ok(""), ok(""), ok("")},
		},
		{
			name: "hold",
			body: holdRow,
			// L0 refuses it for being unattended at all, not for the hold.
			want: [4]want{denied("runs.interactive"), ok("hold"), ok("hold"), ok("hold")},
		},
		{
			name: "auto",
			body: autoRow,
			// L1 overrides the caller's explicit `auto`. That override IS the
			// rung: a member who could opt out with one field would make the
			// rubric advisory.
			want: [4]want{denied("runs.interactive"), ok("hold"), ok("auto"), ok("auto")},
		},
		{
			name: "seed_auto_tools",
			body: `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true,"seed_auto_tools":true}`,
			// Interactive, so only the seed can refuse it — the pre-attach span
			// runs skip-permissions with no toolgate and no human at the pane.
			want: [4]want{denied("runs.seed_auto_tools"), denied("runs.seed_auto_tools"), ok(""), ok("")},
		},
		{
			name: "exec",
			body: `{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`,
			// The door that routes around every other gate, so it is the top
			// rung's alone — including at L0, which the ladder's shape gives
			// for free and a per-rung switch would have missed.
			want: [4]want{denied("runs.task_mode"), denied("runs.task_mode"), denied("runs.task_mode"), ok("")},
		},
		{
			name: "codex-cli non-interactive",
			body: `{"agent":"codex-cli","task":"t","confinement_class":"CC2"}`,
			// L1 is where it bites: the rung would derive a hold, and an agent
			// with no external tool-approval contract cannot honour one, so the
			// run is refused rather than shipped unsupervised. L2/L3 derive
			// nothing and the agent is unremarkable again.
			want: [4]want{denied("runs.interactive"), denied("runs.agent"), ok(""), ok("")},
		},
	} {
		for i, level := range []types.AutonomyLevel{types.AutonomyL0, types.AutonomyL1, types.AutonomyL2, types.AutonomyL3} {
			t.Run(shape.name+"/"+string(level), func(t *testing.T) {
				srv, st, audit := govEscapeFixture(t, autonomyCapStore(autonomyProfile(level)))
				w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-autonomy", []string{"eng"}, false), shape.body)
				exp := shape.want[i]
				if w.Code != exp.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, exp.status, w.Body.String())
				}
				if exp.status == http.StatusForbidden {
					if got := lastAuthzDenied(audit.events); got != exp.target {
						t.Errorf("authz.denied target = %q, want %q", got, exp.target)
					}
					st.mu.Lock()
					runs := len(st.runs)
					st.mu.Unlock()
					if runs != 0 {
						t.Errorf("a refused request left %d run row(s) behind", runs)
					}
					return
				}
				data := autonomyCreateAudit(t, st, audit)
				if got, _ := data["tool_approvals"].(string); got != exp.toolApprovals {
					t.Errorf("audited tool_approvals = %q, want %q", got, exp.toolApprovals)
				}
				a, _ := data["autonomy"].(map[string]any)
				if a == nil {
					t.Fatalf("run.create carries no autonomy provenance: %v", data)
				}
				if got, _ := a["level"].(string); got != string(level) {
					t.Errorf("audited autonomy level = %q, want %q", got, level)
				}
			})
		}
	}
}

// TestRunAutonomyFreezesTheLevelOnTheRun pins the OTHER provenance sink: the
// level is frozen on the run row (migration 0065), so a run's autonomy survives
// the audit stream's retention and is readable from the run itself.
func TestRunAutonomyFreezesTheLevelOnTheRun(t *testing.T) {
	srv, st, _ := govEscapeFixture(t, autonomyCapStore(autonomyProfile(types.AutonomyL2)))
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-autonomy", []string{"eng"}, false),
		`{"agent":"claude-code","task":"t","confinement_class":"CC2"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	var got types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if got.AutonomyLevel != types.AutonomyL2 {
		t.Errorf("run.autonomy_level = %q, want %q", got.AutonomyLevel, types.AutonomyL2)
	}
	st.mu.Lock()
	stored := st.runs[got.ID].AutonomyLevel
	st.mu.Unlock()
	if stored != types.AutonomyL2 {
		t.Errorf("stored autonomy_level = %q, want %q", stored, types.AutonomyL2)
	}
}

// ─── Review and launch answer with the same object ────────────────────────────

// TestAutonomyReviewMatchesLaunch is the property the whole design is shaped
// around: POST /runs/preflight returns the SAME autonomy object POST /runs
// audits, for the same body.
//
// Compared as decoded JSON rather than field by field, deliberately: a field
// added to AutonomyResolution and threaded to one door only is exactly the
// drift this asserts against, and a field-by-field comparison would not see it.
//
// TestPreflightMirrorsLaunchGates already proves both handlers CALL the gate.
// This proves they get the same answer out of it — which the structural guard
// cannot see, because the two doors reach the call with independently built
// specs (launch's is pre-union, Review's has already been widened by the
// workspace egress lanes; autonomyPostureSpec is what reconciles them).
func TestAutonomyReviewMatchesLaunch(t *testing.T) {
	const body = `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`
	profile := autonomyProfile(types.AutonomyL1)
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }

	srv, _, _ := govEscapeFixture(t, autonomyCapStore(profile))
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member(t), body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Autonomy map[string]any `json:"autonomy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if resp.Autonomy == nil {
		t.Fatalf("preflight published no autonomy object: %s", w.Body.String())
	}

	srv2, st2, audit2 := govEscapeFixture(t, autonomyCapStore(profile))
	if c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), body); c.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
	}
	launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)
	if launched == nil {
		t.Fatalf("run.create carries no autonomy provenance")
	}
	review, _ := json.Marshal(resp.Autonomy)
	audited, _ := json.Marshal(launched)
	if string(review) != string(audited) {
		t.Errorf("Review and launch disagree about the run's autonomy:\n  review = %s\n  launch = %s", review, audited)
	}
	// And the posture is the fixture's, spelled out — so a change to the
	// clamped spec or the enforced class reds HERE, naming what moved, instead
	// of silently making the equality above compare two identically wrong
	// objects.
	posture, _ := launched["posture"].(map[string]any)
	for field, want := range map[string]string{"egress": "sealed", "secrets": "none", "confinement": "CC2"} {
		if got, _ := posture[field].(string); got != want {
			t.Errorf("posture.%s = %q, want %q (fixture: %v)", field, got, want, posture)
		}
	}
}

// TestAutonomyReviewRefusesWhatLaunchRefuses is the same parity on the other
// outcome: a shape the rung refuses must be refused on Review too, with the
// same status and the same target, or the member meets the refusal only after
// clicking launch.
func TestAutonomyReviewRefusesWhatLaunchRefuses(t *testing.T) {
	const body = `{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`
	profile := autonomyProfile(types.AutonomyL2)

	srv, _, audit := govEscapeFixture(t, autonomyCapStore(profile))
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", govSession(t, "sub-autonomy", []string{"eng"}, false), body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("preflight = %d, want 403: %s", w.Code, w.Body.String())
	}
	if got := lastAuthzDenied(audit.events); got != "runs.task_mode" {
		t.Errorf("preflight authz.denied target = %q, want runs.task_mode", got)
	}
}

// TestAutonomyPostureIncludesWorkspaceEgressAtBothDoors is the parity case the
// two handlers are structurally most likely to get wrong, and the reason
// autonomyPostureSpec exists.
//
// An onboarded workspace's approved egress reaches a run through a union that
// runs at DIFFERENT points on the two doors: preflight widens the spec before
// it folds anything, while launch's unionRunEgress runs after the create audit
// row is written — i.e. after this gate. Graded as each handler's spec stands,
// the same request is `open` on Review and `sealed` at launch, and with the
// rubric below that is not a cosmetic difference: Review would show L1 while
// launch handed the member L3, the rung that permits `task_mode=exec`.
//
// The rubric therefore names the two egress postures with DIFFERENT levels, so
// a regression shows up as a wrong level and not merely a wrong label.
func TestAutonomyPostureIncludesWorkspaceEgressAtBothDoors(t *testing.T) {
	const beyondBaselineHost = "forge.corp.example"
	// workspace_repos, the same second door into the workspace lane row 10 of
	// the escape table uses; the repo must be ONBOARDED or the request 422s
	// before this gate is reached.
	body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],"workspace_repos":[{"repo":"` + govWorkspaceRepo + `"}]}}`
	profile := govProfile("workspace-egress")
	profile.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
		EgressOpen: types.AutonomyL1, EgressSealed: types.AutonomyL3,
	}}
	workspaces := []types.Workspace{{
		ID: uuid.New(), Name: "hello",
		Sources:        []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}},
		ApprovedEgress: []string{beyondBaselineHost},
	}}
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }

	srv, st, _ := govEscapeFixture(t, autonomyCapStore(profile))
	st.workspaces = workspaces
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member(t), body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Autonomy map[string]any `json:"autonomy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}

	srv2, st2, audit2 := govEscapeFixture(t, autonomyCapStore(profile))
	st2.workspaces = workspaces
	if c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), body); c.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
	}
	launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)

	for _, side := range []struct {
		name string
		got  map[string]any
	}{{"review", resp.Autonomy}, {"launch", launched}} {
		if side.got == nil {
			t.Fatalf("%s published no autonomy object", side.name)
		}
		posture, _ := side.got["posture"].(map[string]any)
		if got, _ := posture["egress"].(string); got != string(types.AutonomyEgressOpen) {
			t.Errorf("%s: posture.egress = %q, want open — the workspace's approved host %q is beyond the safe baseline",
				side.name, got, beyondBaselineHost)
		}
		if got, _ := side.got["level"].(string); got != string(types.AutonomyL1) {
			t.Errorf("%s: level = %q, want L1 (the open-egress cap)", side.name, got)
		}
	}
}

// ─── the absent-row rule ──────────────────────────────────────────────────────

// TestAutonomyAbsentRowChangesNothing pins the promise every GovernanceLimits
// field makes and this one has the most to lose by breaking: a member with no
// assigned profile, and a member under a profile carrying no rubric, get
// EXACTLY what they got before this gate existed.
//
// The bodies are the two the ladder refuses hardest — an unattended `auto` run
// and an exec run — so a gate that leaked past its `Profile == nil` /
// `AutonomyRubric == nil` guard would 403 here rather than merely mis-grade.
// The audit row is checked for the ABSENCE of the key, which is what keeps the
// payload byte-for-byte what a deployment that authors no rubric has always
// had.
func TestAutonomyAbsentRowChangesNothing(t *testing.T) {
	noRubric := govProfile("no-rubric")
	noRubric.Limits = types.GovernanceLimits{} // an assigned profile, no rubric on it

	for _, fixture := range []struct {
		name  string
		store *capStore
	}{
		{"no assigned profile", &capStore{}},
		{"an assigned profile with no rubric", autonomyCapStore(noRubric)},
	} {
		for _, body := range []string{
			`{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`,
			`{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`,
			`{"agent":"codex-cli","task":"t","confinement_class":"CC2"}`,
		} {
			t.Run(fixture.name+"/"+body, func(t *testing.T) {
				srv, st, audit := govEscapeFixture(t, fixture.store)
				w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
					govSession(t, "sub-autonomy", []string{"eng"}, false), body)
				if w.Code != http.StatusCreated {
					t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
				}
				data := autonomyCreateAudit(t, st, audit)
				if _, present := data["autonomy"]; present {
					t.Errorf("run.create grew an autonomy key for a run nothing bound: %v", data["autonomy"])
				}
				var run types.AgentRun
				if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
					t.Fatalf("decode run: %v", err)
				}
				if run.AutonomyLevel != "" {
					t.Errorf("autonomy_level = %q, want empty", run.AutonomyLevel)
				}
				pf := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight",
					govSession(t, "sub-autonomy", []string{"eng"}, false), body)
				if pf.Code != http.StatusOK {
					t.Fatalf("preflight = %d, want 200: %s", pf.Code, pf.Body.String())
				}
				if bytesContainsKey(pf.Body.Bytes(), "autonomy") {
					t.Errorf("preflight grew an autonomy key for a run nothing bound: %s", pf.Body.String())
				}
			})
		}
	}
}

// TestAutonomyRubricThatCapsNothingChangesNothing is the third absent-row
// shape, and the one a `!= nil` check alone gets wrong: a rubric that EXISTS
// but leaves this posture's three fields unset caps nothing, identically to no
// rubric at all (AutonomyRubric's own doc). It must not refuse, must not
// derive, and must not publish provenance for a decision nobody made.
func TestAutonomyRubricThatCapsNothingChangesNothing(t *testing.T) {
	p := govProfile("rubric-elsewhere")
	// The fixture's posture is sealed/none/CC2; every field named here applies
	// to some OTHER posture.
	p.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
		EgressOpen: types.AutonomyL0, SecretsPowerful: types.AutonomyL0, ConfinementCC1: types.AutonomyL0,
	}}
	srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-autonomy", []string{"eng"}, false),
		`{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	data := autonomyCreateAudit(t, st, audit)
	if got, _ := data["tool_approvals"].(string); got != "auto" {
		t.Errorf("tool_approvals = %q — a rubric that caps nothing derived a hold", got)
	}
	if _, present := data["autonomy"]; present {
		t.Errorf("run.create grew an autonomy key for a rubric that bound nothing: %v", data["autonomy"])
	}
}

// bytesContainsKey reports whether a JSON object body carries the named
// top-level key.
func bytesContainsKey(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
