// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// the autonomy fixture

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
	ev := findAudit(audit.snapshot(), runID, "run.create", "success")
	if ev == nil {
		t.Fatalf("no run.create audit row for %s", runID)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("run.create payload is not an object: %v (%s)", err, ev.Data)
	}
	return data
}

// the ladder table

// TestRunAutonomyLadder is the gate's behaviour table: four levels against the
// request shapes the rungs are defined in terms of, driven end to end through
// POST /runs as an assigned member.
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
			body: `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true,"interactive_start":"agent"}`,
			// Permitted at every rung, and NEVER derived to hold: an
			// interactive run refuses an explicit hold by design and dispatch
			// writes WARDYN_TOOL_APPROVALS for non-interactive runs alone, so a
			// derived one there is a field accepted and thrown away. The seed
			// goes to the agent as a prompt, which parks its own approval
			// prompt until a human attaches.
			want: [4]want{ok(""), ok(""), ok(""), ok("")},
		},
		{
			name: "shell boot seed",
			body: `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true}`,
			// interactive_start unset: the image runs the task as `bash -lc`
			// at boot, before anyone attaches — exec's reach, so exec's rung.
			// Ranked with seed_auto_tools instead, L2 would refuse
			// `task_mode=exec` and launch the same command here.
			want: [4]want{denied("runs.interactive_start"), denied("runs.interactive_start"), denied("runs.interactive_start"), ok("")},
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
			body: `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true,"interactive_start":"agent","seed_auto_tools":true}`,
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

// Review and launch answer with the same object

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
	// And this fixture's rubric caps all nine postures at the same rung, so
	// the run is a THREE-WAY tie — the case the byte equality above is least
	// able to police on its own. A fold that kept one cause would still make
	// the two sides equal (both wrong, identically), so the tie is spelled out
	// here and the parity assertion keeps the two doors on it together.
	for _, side := range []struct {
		name string
		got  map[string]any
	}{{"review", resp.Autonomy}, {"launch", launched}} {
		if got := autonomyBoundBy(t, side.got); !slices.Equal(got, []string{"egress_sealed", "secrets_none", "confinement_cc2"}) {
			t.Errorf("%s: bound_by = %v, want all three tied causes in field order", side.name, got)
		}
	}
}

// autonomyBoundBy pulls bound_by out of a decoded resolution.
func autonomyBoundBy(t *testing.T, resolution map[string]any) []string {
	t.Helper()
	raw, _ := resolution["bound_by"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("bound_by carries a non-string cause %v (%T)", v, v)
		}
		out = append(out, s)
	}
	return out
}

// every tied cause is named

// TestAutonomyBoundByNamesEveryTiedCause is the #96 wire ruling, asserted
// where it is load-bearing: bound_by is a LIST, and a member capped by a
// three-way tie is told all three rows rather than the first in a fixed order.
//
// The counterfactual is what makes this a test and not a preference. A fold
// that kept one cause passes every other case in this file unchanged — same
// level, same refusal, same Review/launch parity — while telling an admin to
// raise `egress_sealed`, which moves nothing, because `secrets_none` and
// `confinement_cc2` cap the run at the same rung. So both halves are asserted:
// the SENTENCE the member reads and the ARRAY the console and the audit row
// read, and a losing cap is checked for its absence beside the winners.
func TestAutonomyBoundByNamesEveryTiedCause(t *testing.T) {
	// govEscapeFixture's posture is sealed / none / CC2; exec is refused at
	// every rung below L3, which is what puts the clause in front of a member.
	const execBody = `{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`
	const okBody = `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true,"interactive_start":"agent"}`
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }

	for _, tc := range []struct {
		name    string
		rubric  *types.AutonomyRubric
		clause  string
		boundBy []string
	}{
		{
			name:    "one cause stands alone",
			rubric:  &types.AutonomyRubric{EgressSealed: types.AutonomyL1},
			clause:  "(bound by egress_sealed)",
			boundBy: []string{"egress_sealed"},
		},
		{
			name:    "two tied causes are joined with and",
			rubric:  &types.AutonomyRubric{EgressSealed: types.AutonomyL1, ConfinementCC2: types.AutonomyL1},
			clause:  "(bound by egress_sealed and confinement_cc2)",
			boundBy: []string{"egress_sealed", "confinement_cc2"},
		},
		{
			name:    "three tied causes are all named, in field order",
			rubric:  autonomyRubric(types.AutonomyL1),
			clause:  "(bound by egress_sealed, secrets_none and confinement_cc2)",
			boundBy: []string{"egress_sealed", "secrets_none", "confinement_cc2"},
		},
		{
			// A row that caps HIGHER than the resolved level is not a cause and
			// must not ride along: naming it would send an admin to edit the one
			// row that is already permissive enough.
			name: "a losing cap is not named beside the winners",
			rubric: &types.AutonomyRubric{
				EgressSealed: types.AutonomyL3, SecretsNone: types.AutonomyL1, ConfinementCC2: types.AutonomyL1,
			},
			clause:  "(bound by secrets_none and confinement_cc2)",
			boundBy: []string{"secrets_none", "confinement_cc2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("autonomy-tie")
			p.Limits = types.GovernanceLimits{AutonomyRubric: tc.rubric}

			srv, _, _ := govEscapeFixture(t, autonomyCapStore(p))
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member(t), execBody)
			if w.Code != http.StatusForbidden {
				t.Fatalf("create = %d, want 403: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.clause) {
				t.Errorf("the refusal does not name every tied cause:\n  want substring %q\n  got %s", tc.clause, w.Body.String())
			}

			// The same resolution on the wire, at both doors, for a request
			// this rung permits.
			srv2, st2, audit2 := govEscapeFixture(t, autonomyCapStore(p))
			if c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), okBody); c.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
			}
			launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)
			if launched == nil {
				t.Fatalf("run.create carries no autonomy provenance")
			}
			pf := doSSO(t, srv2, http.MethodPost, "/api/v1/runs/preflight", member(t), okBody)
			if pf.Code != http.StatusOK {
				t.Fatalf("preflight = %d, want 200: %s", pf.Code, pf.Body.String())
			}
			var resp struct {
				Autonomy map[string]any `json:"autonomy"`
			}
			if err := json.Unmarshal(pf.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode preflight: %v", err)
			}
			for _, side := range []struct {
				name string
				got  map[string]any
			}{{"launch", launched}, {"review", resp.Autonomy}} {
				if side.got == nil {
					t.Fatalf("%s published no autonomy object", side.name)
				}
				if got := autonomyBoundBy(t, side.got); !slices.Equal(got, tc.boundBy) {
					t.Errorf("%s: bound_by = %v, want %v", side.name, got, tc.boundBy)
				}
			}
		})
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

// TestAutonomyPostureIncludesSiteConfigScmHostsAtBothDoors is the OTHER lane
// unionRunEgress adds after the gate, and the one that is not grant-dependent
// at all — which is what makes it an escape rather than a rounding error.
//
// `declaresRepo` (runs_create.go) is true from the legacy free-text `repo`
// field alone, and unionSiteConfigScmHosts reads nothing but site config. So a
// member reaches an operator-declared internal forge with no grant, no
// workspace and no approval; a posture blind to that host grades the run
// `sealed`, and the rubric below hands `sealed` the rung that runs tool calls
// unsupervised. The two doors AGREE on that wrong answer, so the parity test
// cannot see it — only grading the host can.
//
// The second row is the scope: a run that declares no repo inherits no SCM
// lane at launch either, so grading it `open` would cap runs on reach they can
// never have.
func TestAutonomyPostureIncludesSiteConfigScmHostsAtBothDoors(t *testing.T) {
	const ghes = "ghes.corp.example"
	profile := govProfile("scm-egress")
	profile.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
		EgressOpen: types.AutonomyL1, EgressSealed: types.AutonomyL3,
	}}
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }

	for _, tc := range []struct {
		name       string
		body       string
		wantEgress types.AutonomyEgressPosture
		wantLevel  types.AutonomyLevel
	}{
		{
			name:       "a declared repo inherits the operator's SCM host, so the run is open",
			body:       `{"agent":"claude-code","task":"t","confinement_class":"CC2","repo":"https://` + ghes + `/team/app"}`,
			wantEgress: types.AutonomyEgressOpen,
			wantLevel:  types.AutonomyL1,
		},
		{
			name:       "a run that declares no repo inherits no SCM lane and stays sealed",
			body:       `{"agent":"claude-code","task":"t","confinement_class":"CC2"}`,
			wantEgress: types.AutonomyEgressSealed,
			wantLevel:  types.AutonomyL3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, autonomyCapStore(profile))
			st.siteConfig = types.SiteConfig{ScmHosts: []string{ghes}}
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member(t), tc.body)
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
			st2.siteConfig = types.SiteConfig{ScmHosts: []string{ghes}}
			if c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), tc.body); c.Code != http.StatusCreated {
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
				if got, _ := posture["egress"].(string); got != string(tc.wantEgress) {
					t.Errorf("%s: posture.egress = %q, want %q (the operator's SCM host is %q)",
						side.name, got, tc.wantEgress, ghes)
				}
				if got, _ := side.got["level"].(string); got != string(tc.wantLevel) {
					t.Errorf("%s: level = %q, want %q", side.name, got, tc.wantLevel)
				}
			}
		})
	}
}

// TestAutonomyPostureIncludesGrantLanesAtBothDoors is the grant-opened half of
// the same property: three lanes unionRunEgress adds at launch because of a
// GRANT, each graded on both doors from the spec alone.
//
// The secrets axis cannot stand in for them. A read-only github_token grades
// `baseline`, not `powerful`, yet it declares a repo and so inherits the
// operator's SCM hosts; and even a `powerful` grade bounds nothing unless an
// admin happens to rank secrets_powerful at or below egress_open. So the
// rubric below names the egress rows only, and every row must come back `open`
// on reach the grant alone opened.
func TestAutonomyPostureIncludesGrantLanesAtBothDoors(t *testing.T) {
	const ghes = "ghes.corp.example"
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }

	for _, tc := range []struct {
		name     string
		grant    types.GrantSpec
		scmHosts []string
	}{
		{
			name:     "a read-only github_token declares a repo, so the operator's SCM host is reach",
			grant:    types.GrantSpec{Kind: types.GrantGitHubToken, Scope: mustJSON(map[string]any{"repos": []string{"acme/widgets"}})},
			scmHosts: []string{ghes},
		},
		{
			name:  "a git_pat to Azure DevOps opens the ADO bundle",
			grant: types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "dev.azure.com", "secret_name": govCorpSecret})},
		},
		{
			name:  "an ssh_key opens its SSH-over-443 endpoint",
			grant: types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": govCorpSecret})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("grant-egress")
			p.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
				EgressOpen: types.AutonomyL1, EgressSealed: types.AutonomyL3,
			}}
			p.Ceiling.EligibleGrants = []types.GrantSpec{tc.grant}
			body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
				`"allowed_domains":["api.anthropic.com"],"eligible_grants":[` + string(mustJSON(tc.grant)) + `]}}`
			fixture := func() (*Server, *govEscapeStore, *recRecorder) {
				srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
				srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{tc.grant}
				st.siteConfig = types.SiteConfig{ScmHosts: tc.scmHosts}
				return srv, st, audit
			}

			srv, _, _ := fixture()
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

			srv2, st2, audit2 := fixture()
			if c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), body); c.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
			}
			launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)

			review, _ := json.Marshal(resp.Autonomy)
			audited, _ := json.Marshal(launched)
			if string(review) != string(audited) {
				t.Errorf("Review and launch disagree:\n  review = %s\n  launch = %s", review, audited)
			}
			posture, _ := launched["posture"].(map[string]any)
			if got, _ := posture["egress"].(string); got != string(types.AutonomyEgressOpen) {
				t.Errorf("posture.egress = %q, want open: the grant alone opened a lane beyond the safe baseline", got)
			}
			if got, _ := launched["level"].(string); got != string(types.AutonomyL1) {
				t.Errorf("level = %q, want L1 (the open-egress cap)", got)
			}
		})
	}
}

// the absent-row rule

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

// the two sentences on the 201

// TestAutonomyWarningsOnTheCreatedRun pins the gate's advisory half, which the
// audit row cannot speak for: the run was CREATED, so the only thing that
// reaches the person who launched it is the 201's warnings list.
//
// Two sentences, each with its own silence condition, and the silences are the
// half worth testing. A derived hold is reported because the member asked for
// `auto` and did not get it — but a member who ASKED for hold was told nothing
// new, and a warning there would train people to ignore the channel. The
// missing-lane sentence is the honest one: at any rung, an agent with no
// in-sandbox tool-approval half means the level is enforced at this door and at
// the proxy and by nothing inside the container — and it is skipped for an exec
// run, which has no agent process to say it about.
func TestAutonomyWarningsOnTheCreatedRun(t *testing.T) {
	const derived = "tool_approvals was set to hold"
	const noLane = "has no Wardyn tool-approval lane"
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }

	for _, tc := range []struct {
		name   string
		level  types.AutonomyLevel
		body   string
		want   []string
		absent []string
	}{
		{
			name:   "an overridden auto is reported, with the profile and every tied cause",
			level:  types.AutonomyL1,
			body:   `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`,
			want:   []string{derived, `governance profile "autonomy-warnings"`, "L1", "bound by egress_sealed, secrets_none and confinement_cc2"},
			absent: []string{noLane},
		},
		{
			// Nothing was derived: the caller already asked for the supervision
			// the rung requires.
			name:   "an explicit hold is not reported back as a derivation",
			level:  types.AutonomyL1,
			body:   `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"hold"}`,
			absent: []string{derived, noLane},
		},
		{
			// Permitted at L2 (nothing is derived, so nothing is refused), and
			// the level is still enforced by nothing inside that container.
			name:   "an agent with no agent-side layer is named at a rung that permits it",
			level:  types.AutonomyL2,
			body:   `{"agent":"codex-cli","task":"t","confinement_class":"CC2"}`,
			want:   []string{noLane, "codex-cli", "L2"},
			absent: []string{derived},
		},
		{
			// No agent process to say it about, and an exec run may legitimately
			// name no agent at all.
			name:   "an exec run is not told about an agent-side layer it has no agent for",
			level:  types.AutonomyL3,
			body:   `{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`,
			absent: []string{derived, noLane},
		},
		{
			// The absent row owes the caller no sentence at all.
			name:   "a rubric that caps nothing says nothing",
			body:   `{"agent":"codex-cli","task":"t","confinement_class":"CC2"}`,
			absent: []string{derived, noLane},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("autonomy-warnings")
			if tc.level != "" {
				p.Limits = types.GovernanceLimits{AutonomyRubric: autonomyRubric(tc.level)}
			}
			srv, _, _ := govEscapeFixture(t, autonomyCapStore(p))
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member(t), tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
			}
			var resp createRunResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			all := strings.Join(resp.Warnings, "\n")
			for _, want := range tc.want {
				if !strings.Contains(all, want) {
					t.Errorf("the 201 does not say %q; warnings were:\n%s", want, all)
				}
			}
			for _, no := range tc.absent {
				if strings.Contains(all, no) {
					t.Errorf("the 201 says %q and should not; warnings were:\n%s", no, all)
				}
			}
		})
	}
}

// TestAutonomyUndefinedLevelFailsClosed is the corrupted-column case: a stored
// rubric carrying a level no AutonomyLevel defines.
//
// Unreachable through the API — governanceLimitsRefusal validates the nine
// fields at write — so the only way in is a hand-edited column or a future
// level a downgraded binary does not know. Either way the gate must fail
// CLOSED. It does so structurally rather than by a special case:
// AutonomyLevel.Rank() is -1 for an unrecognised value, which is below L0, so
// the undefined cap wins the minimum and every threshold in the ladder refuses.
//
// The resolution still reports the stored value verbatim. Clamping it to a real
// rung would be inventing a level nobody authored, and an operator reading the
// audit row needs to see the string that is actually in their column.
func TestAutonomyUndefinedLevelFailsClosed(t *testing.T) {
	p := govProfile("autonomy-corrupt")
	p.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{EgressSealed: "L9"}}
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }

	srv, st, _ := govEscapeFixture(t, autonomyCapStore(p))
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member(t),
		`{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an unattended run under an undefined level = %d, want 403: %s", w.Code, w.Body.String())
	}
	st.mu.Lock()
	runs := len(st.runs)
	st.mu.Unlock()
	if runs != 0 {
		t.Errorf("the refused run left %d row(s) behind", runs)
	}

	// The most supervised shape there is still launches: failing closed means
	// refusing what is unattended, not bricking the profile.
	srv2, st2, audit2 := govEscapeFixture(t, autonomyCapStore(p))
	c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t),
		`{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true,"interactive_start":"agent"}`)
	if c.Code != http.StatusCreated {
		t.Fatalf("an interactive run = %d, want 201: %s", c.Code, c.Body.String())
	}
	launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)
	if got, _ := launched["level"].(string); got != "L9" {
		t.Errorf("audited level = %q, want the stored value %q carried through verbatim", got, "L9")
	}
}

// the per-person Azure DevOps lane (#474)

// TestAutonomyPostureGradesTheADOEntraCredentialAtCreate is the security
// review's probe, kept: the posture graded at create for a run on the
// per-person Azure DevOps lane, against the same run once dispatch has written
// the api_key grants createADOEntraGrants authors for it.
//
// The two must fold to the SAME level, and the reason is the whole gate: the
// level is frozen at create (resolveRunAutonomy) and the credential is
// authored at dispatch (authorADOEntraLane), so a secrets axis reading
// spec.EligibleGrants alone graded this run `none` — and launched it on the
// autonomous rung while it carried the person's Entra bearer proxy-side.
//
// The rubric names the secrets rows apart from the egress one so a regression
// shows up as a wrong LEVEL, not merely a wrong label: the workspace's own
// clone host already makes this run `open`, and with every row at one level
// the miss would be invisible.
func TestAutonomyPostureGradesTheADOEntraCredentialAtCreate(t *testing.T) {
	site := adoSite(adoEntraTestRow())
	adoRun, ok := resolveADOEntraRun(site, []string{adoTestRepo}, adoTestOwner)
	if !ok {
		t.Fatalf("fixture: %q does not resolve to the per-person Azure DevOps lane", adoTestRepo)
	}
	grade := adoEntraGradedAs(adoRun, ok)
	ws := types.Workspace{ID: uuid.New(), Name: "ado",
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}}}
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"},
		WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}},
	}
	rubric := types.AutonomyRubric{
		EgressOpen: types.AutonomyL2, SecretsNone: types.AutonomyL2,
		SecretsBaseline: types.AutonomyL2, SecretsPowerful: types.AutonomyL1,
	}

	graded := composer.AutonomyPostureOf(autonomyPostureSpec(spec, []types.Workspace{ws}, "", site, grade), types.CC2)
	level, boundBy := composer.FoldAutonomy(rubric, graded)

	// The same spec as dispatch leaves it: the grants the lane really writes.
	dispatched := spec
	dispatched.EligibleGrants = adoEntraPostureGrants("contoso")
	after := composer.AutonomyPostureOf(autonomyPostureSpec(dispatched, []types.Workspace{ws}, "", site, grade), types.CC2)
	afterLevel, afterBound := composer.FoldAutonomy(rubric, after)

	if graded.Secrets != types.AutonomySecretsPowerful {
		t.Errorf("posture.secrets at create = %q, want powerful: this run will hold an api_key to dev.azure.com, "+
			"which is outside composer.safeBaselineDomains", graded.Secrets)
	}
	if level != afterLevel {
		t.Errorf("level graded at create = %s (bound by %v), but the run carries the dispatch-written credential "+
			"and grades %s (bound by %v)", level, boundBy, afterLevel, afterBound)
	}
	if level != types.AutonomyL1 {
		t.Errorf("level = %s, want L1 — the rubric's secrets_powerful cap", level)
	}
}

// TestAutonomyPostureIncludesTheADOEntraLaneAtBothDoors is the grant-lane
// property for the one lane whose grant does not exist yet at either door.
//
// It sits beside TestAutonomyPostureIncludesGrantLanesAtBothDoors and asks a
// strictly harder question. Those lanes are opened by a grant the REQUEST
// carries, so both doors can see it; this one is opened by a provider row and
// the caller's own identity, and the grant is written at dispatch. Review and
// launch therefore agreed with each other on the understated posture, which is
// exactly what a parity assertion alone cannot catch — so every row below
// asserts the posture and the level as well as the parity.
//
// The second and third rows are the scope. A run whose Azure DevOps row is not
// on the per-person lane, and a run on a deployment with no provider rows at
// all, must be graded byte-for-byte what they were before this fold existed;
// grading them powerful would cap ordinary runs on a credential they never get.
func TestAutonomyPostureIncludesTheADOEntraLaneAtBothDoors(t *testing.T) {
	sharedRow := adoEntraTestRow()
	sharedRow.CredentialSource = types.CredentialSourceShared

	for _, tc := range []struct {
		name        string
		site        types.SiteConfig
		wantSecrets types.AutonomySecretsPosture
		wantLevel   types.AutonomyLevel
	}{
		{
			name:        "the per-person lane resolves, so the run is graded on the credential dispatch writes",
			site:        adoSite(adoEntraTestRow()),
			wantSecrets: types.AutonomySecretsPowerful,
			wantLevel:   types.AutonomyL1,
		},
		{
			name:        "an Azure DevOps row whose credential is shared authors no lane",
			site:        adoSite(sharedRow),
			wantSecrets: types.AutonomySecretsNone,
			wantLevel:   types.AutonomyL2,
		},
		{
			name:        "a deployment with no provider rows is graded exactly as before",
			site:        types.SiteConfig{},
			wantSecrets: types.AutonomySecretsNone,
			wantLevel:   types.AutonomyL2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("ado-entra")
			p.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
				EgressOpen: types.AutonomyL2, SecretsNone: types.AutonomyL2,
				SecretsBaseline: types.AutonomyL2, SecretsPowerful: types.AutonomyL1,
			}}
			body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
				`"allowed_domains":["api.anthropic.com"],"workspace_repos":[{"repo":"` + adoTestRepo + `"}]}}`
			workspaces := []types.Workspace{{ID: uuid.New(), Name: "ado",
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}}}}
			// The session subject IS the lane's owner: dispatch resolves the row
			// from runIdentitySubject(run.CreatedBy), and this gate has to ask
			// the identical question of the identical person.
			member := func(t *testing.T) *http.Cookie { return govSession(t, adoTestOwner, []string{"eng"}, false) }
			fixture := func() (*Server, *govEscapeStore, *recRecorder) {
				srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
				st.workspaces = workspaces
				st.siteConfig = tc.site
				return srv, st, audit
			}

			srv, _, _ := fixture()
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

			srv2, st2, audit2 := fixture()
			c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), body)
			if c.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
			}
			launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)

			// The 201 warning has to NAME the credential when it is what graded
			// powerful. The member's request declared no secret at all on this
			// lane, so "narrow the run's secrets" is unactionable without the
			// noun, and the admin has nothing to look up either.
			if tc.wantSecrets == types.AutonomySecretsPowerful {
				var created struct {
					Warnings []string `json:"warnings"`
				}
				if err := json.Unmarshal(c.Body.Bytes(), &created); err != nil {
					t.Fatalf("decode create: %v", err)
				}
				if !slices.ContainsFunc(created.Warnings, func(s string) bool { return strings.Contains(s, "contoso") }) {
					t.Errorf("no 201 warning names the Azure DevOps organisation that graded this run powerful: %q", created.Warnings)
				}
			}

			review, _ := json.Marshal(resp.Autonomy)
			audited, _ := json.Marshal(launched)
			if string(review) != string(audited) {
				t.Errorf("Review and launch disagree:\n  review = %s\n  launch = %s", review, audited)
			}
			posture, _ := launched["posture"].(map[string]any)
			if got, _ := posture["secrets"].(string); got != string(tc.wantSecrets) {
				t.Errorf("posture.secrets = %q, want %q", got, tc.wantSecrets)
			}
			if got, _ := launched["level"].(string); got != string(tc.wantLevel) {
				t.Errorf("level = %q, want %q", got, tc.wantLevel)
			}
		})
	}
}
