// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The org's autonomy cap on runs that carry a self-defined component
// (components.autonomy_cap). The component gate that fills runComponents lands
// in a later change, so these drive resolveRunAutonomy directly, with the
// decision the gate will pass — the one function both doors call.

// componentCapShapes are TestRunAutonomyLadder's request shapes, with the
// L0 and L1 columns of that table: the cap is "exactly the ladder", so with no
// profile at all it must answer what a rubric resolving to the same rung does.
var componentCapShapes = []struct {
	name string
	body string
	// L0, L1: "" = permitted, else the authz.denied target
	deny [2]string
	// the tool_approvals the request leaves the gate with when permitted
	toolApprovals [2]string
}{
	{"interactive", `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true,"interactive_start":"agent"}`,
		[2]string{"", ""}, [2]string{"", ""}},
	{"shell boot seed", `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true}`,
		[2]string{"runs.interactive_start", "runs.interactive_start"}, [2]string{}},
	{"hold", `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"hold"}`,
		[2]string{"runs.interactive", ""}, [2]string{"", "hold"}},
	{"auto", `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`,
		[2]string{"runs.interactive", ""}, [2]string{"", "hold"}},
	{"seed_auto_tools", `{"agent":"claude-code","task":"t","confinement_class":"CC2","interactive":true,"interactive_start":"agent","seed_auto_tools":true}`,
		[2]string{"runs.seed_auto_tools", "runs.seed_auto_tools"}, [2]string{}},
	{"exec", `{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`,
		[2]string{"runs.task_mode", "runs.task_mode"}, [2]string{}},
	{"codex-cli non-interactive", `{"agent":"codex-cli","task":"t","confinement_class":"CC2"}`,
		[2]string{"runs.interactive", "runs.agent"}, [2]string{}},
}

// componentCapHelp is the deployment's policy_help in the fixture, so a
// refusal that names the deployment is told apart from one that names nothing.
var componentCapHelp = policyref.Contact{Owner: "Platform team", Email: "platform@corp.example"}

// componentCapResult is one call of the gate.
type componentCapResult struct {
	ok       bool
	res      types.AutonomyResolution
	warnings []string
	req      createRunRequest
	ado      adoEntraGrade
	bedrock  bedrockCredGrade
	code     int
	body     errorBody
	written  bool
	denied   string // the last authz.denied target
}

// callComponentCap runs resolveRunAutonomy for a member, on an empty spec at
// CC2, with ceiling and comps as given.
func callComponentCap(t *testing.T, ceiling governanceCeiling, comps runComponents, body string) componentCapResult {
	t.Helper()
	srv, st, audit := govEscapeFixture(t, &capStore{})
	st.siteConfig = types.SiteConfig{PolicyHelp: &componentCapHelp}
	var req createRunRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(govMemberCtx([]string{"eng"}, false))
	w := httptest.NewRecorder()
	out := componentCapResult{}
	out.res, out.warnings, _, out.ado, out.bedrock, out.ok = srv.resolveRunAutonomy(w, r, &req,
		types.RunPolicySpec{}, nil, types.CC2, ceiling, modelCredentialFacts{}, comps)
	out.req, out.code, out.written = req, w.Code, w.Body.Len() > 0
	if out.written {
		if err := json.Unmarshal(w.Body.Bytes(), &out.body); err != nil {
			t.Fatalf("refusal body is not an error object: %v (%s)", err, w.Body.String())
		}
	}
	out.denied = lastAuthzDenied(audit.snapshot())
	return out
}

func selfComponent(capLevel types.AutonomyLevel) runComponents {
	return runComponents{selfDefined: 1, settings: types.ComponentSettings{AutonomyCap: capLevel}}
}

func profileCeiling(rubric *types.AutonomyRubric) governanceCeiling {
	return governanceCeiling{
		Profile: &ResolvedProfile{ID: uuid.New(), Name: "autonomy"},
		Limits:  types.GovernanceLimits{AutonomyRubric: rubric},
	}
}

// sameOutcome compares what the caller can observe of two calls.
func sameOutcome(t *testing.T, got, want componentCapResult) {
	t.Helper()
	g, _ := json.Marshal([]any{got.ok, got.res, got.warnings, got.req, got.ado, got.bedrock, got.code, got.body, got.written, got.denied})
	w, _ := json.Marshal([]any{want.ok, want.res, want.warnings, want.req, want.ado, want.bedrock, want.code, want.body, want.written, want.denied})
	if string(g) != string(w) {
		t.Errorf("outcome differs from the run with no components:\n  got  %s\n  want %s", g, w)
	}
}

// TestAutonomyComponentCapDefaultChangesNothing is the owner's "warn only by
// default": a self-defined component with no cap set, and a cap set on a run
// whose components are all the organisation's, both answer exactly what a run
// with no components does — every shape allowed, nothing derived, no level.
func TestAutonomyComponentCapDefaultChangesNothing(t *testing.T) {
	for _, shape := range componentCapShapes {
		for name, comps := range map[string]runComponents{
			"self-defined, no cap":    selfComponent(""),
			"org component, cap L0":   {settings: types.ComponentSettings{AutonomyCap: types.AutonomyL0}},
			"org component, cap L1":   {settings: types.ComponentSettings{AutonomyCap: types.AutonomyL1}},
			"self-defined, all unset": {selfDefined: 3},
		} {
			t.Run(shape.name+"/"+name, func(t *testing.T) {
				want := callComponentCap(t, governanceCeiling{}, runComponents{}, shape.body)
				if !want.ok || want.written || want.res.Level != "" {
					t.Fatalf("the baseline itself was capped: %+v", want)
				}
				sameOutcome(t, callComponentCap(t, governanceCeiling{}, comps, shape.body), want)
			})
		}
	}
	t.Run("under a profile whose rubric resolves L2", func(t *testing.T) {
		ceiling := profileCeiling(autonomyRubric(types.AutonomyL2))
		const body = `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`
		want := callComponentCap(t, ceiling, runComponents{}, body)
		if want.res.Level != types.AutonomyL2 {
			t.Fatalf("baseline level = %q, want L2", want.res.Level)
		}
		sameOutcome(t, callComponentCap(t, ceiling, selfComponent(""), body), want)
		sameOutcome(t, callComponentCap(t, ceiling, runComponents{settings: types.ComponentSettings{AutonomyCap: types.AutonomyL0}}, body), want)
	})
}

// TestAutonomyComponentCapWithoutAProfile: the cap works with no governance
// profile, and it is the ladder, rung for rung — refused as component_autonomy,
// naming the organisation's rule and pointing at the deployment's policy.
func TestAutonomyComponentCapWithoutAProfile(t *testing.T) {
	for _, shape := range componentCapShapes {
		for i, capLevel := range []types.AutonomyLevel{types.AutonomyL0, types.AutonomyL1} {
			t.Run(shape.name+"/"+string(capLevel), func(t *testing.T) {
				got := callComponentCap(t, governanceCeiling{}, selfComponent(capLevel), shape.body)
				if target := shape.deny[i]; target != "" {
					assertComponentCapRefusal(t, got, target, policyref.SourceDeployment)
					return
				}
				if !got.ok || got.written {
					t.Fatalf("refused (%d %s), want permitted", got.code, got.body.Error)
				}
				if got.res.Level != capLevel || !slices.Equal(got.res.BoundBy, []string{"custom_component"}) {
					t.Errorf("resolution = %+v, want level %s bound by custom_component alone", got.res, capLevel)
				}
				// The posture is recorded although nothing graded the level on it.
				if got.res.Posture != (types.AutonomyPosture{Egress: types.AutonomyEgressSealed, Secrets: types.AutonomySecretsNone, Confinement: types.CC2}) {
					t.Errorf("posture = %+v, want the run's graded posture", got.res.Posture)
				}
				// No rubric graded this run, so dispatch is not held to a grade.
				if got.ado != adoEntraUngraded() || got.bedrock != bedrockCredUngraded() {
					t.Errorf("grades = %+v / %+v, want ungraded", got.ado, got.bedrock)
				}
				if got.req.ToolApprovals != shape.toolApprovals[i] {
					t.Errorf("tool_approvals = %q, want %q", got.req.ToolApprovals, shape.toolApprovals[i])
				}
			})
		}
	}
	t.Run("the derived hold names the rule", func(t *testing.T) {
		got := callComponentCap(t, governanceCeiling{}, selfComponent(types.AutonomyL1),
			`{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`)
		const want = "tool_approvals was set to hold: your organisation's rule for runs that use your own custom components permits " +
			"autonomy level L1 for this run's posture (bound by custom_component), so this run's tool calls wait for your approval instead of running unsupervised"
		if !slices.Contains(got.warnings, want) {
			t.Errorf("warnings = %q, want %q", got.warnings, want)
		}
	})
	t.Run("an operator is capped too, with no policy to name", func(t *testing.T) {
		got := callComponentCap(t, governanceCeiling{Operator: true}, selfComponent(types.AutonomyL1),
			`{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`)
		if got.ok || got.code != http.StatusForbidden || got.body.Reason != "component_autonomy" {
			t.Fatalf("got %d %q, want 403 component_autonomy", got.code, got.body.Reason)
		}
		if got.body.Policy != nil {
			t.Errorf("policy = %+v, want none for an operator", got.body.Policy)
		}
	})
}

// assertComponentCapRefusal checks a refusal the cap alone decided.
func assertComponentCapRefusal(t *testing.T, got componentCapResult, target, policySource string) {
	t.Helper()
	if got.ok || got.code != http.StatusForbidden {
		t.Fatalf("got ok=%v %d, want a 403", got.ok, got.code)
	}
	if got.body.Reason != "component_autonomy" {
		t.Errorf("reason = %q, want component_autonomy", got.body.Reason)
	}
	if got.denied != target {
		t.Errorf("authz.denied target = %q, want %q", got.denied, target)
	}
	if !strings.Contains(got.body.Error, componentAutonomySource) || !strings.Contains(got.body.Error, "(bound by custom_component)") ||
		strings.Contains(got.body.Error, "governance profile") {
		t.Errorf("sentence = %q, want the organisation's rule bound by custom_component, and no profile", got.body.Error)
	}
	if got.body.Policy == nil || got.body.Policy.Source != policySource {
		t.Fatalf("policy = %+v, want source %q", got.body.Policy, policySource)
	}
	if policySource == policyref.SourceDeployment && got.body.Policy.Owner != componentCapHelp.Owner {
		t.Errorf("policy = %+v, want the deployment's policy_help", got.body.Policy)
	}
}

// TestAutonomyComponentCapFoldsWithARubric: under a profile the level is the
// lower of the two, and the reason and the named policy follow what bound it.
func TestAutonomyComponentCapFoldsWithARubric(t *testing.T) {
	const exec = `{"agent":"claude-code","task":"echo hi","confinement_class":"CC2","task_mode":"exec"}`
	const auto = `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`
	rubricCauses := []string{"egress_sealed", "secrets_none", "confinement_cc2"}

	t.Run("the cap is lower: it alone binds", func(t *testing.T) {
		ceiling := profileCeiling(autonomyRubric(types.AutonomyL2))
		got := callComponentCap(t, ceiling, selfComponent(types.AutonomyL1), auto)
		if !got.ok || got.res.Level != types.AutonomyL1 || !slices.Equal(got.res.BoundBy, []string{"custom_component"}) {
			t.Fatalf("got ok=%v %+v, want L1 bound by custom_component alone", got.ok, got.res)
		}
		if got.req.ToolApprovals != "hold" {
			t.Errorf("tool_approvals = %q, want the derived hold", got.req.ToolApprovals)
		}
		// The cap is a site setting: the refusal points at the deployment, not
		// at the profile, whose contact did not set it.
		assertComponentCapRefusal(t, callComponentCap(t, ceiling, selfComponent(types.AutonomyL1), exec), "runs.task_mode", policyref.SourceDeployment)
	})
	t.Run("a rubric that caps nothing: the cap binds", func(t *testing.T) {
		got := callComponentCap(t, profileCeiling(&types.AutonomyRubric{}), selfComponent(types.AutonomyL0), auto)
		if got.ok || got.denied != "runs.interactive" || got.body.Reason != "component_autonomy" {
			t.Fatalf("got ok=%v %q %q, want refused at runs.interactive as component_autonomy", got.ok, got.denied, got.body.Reason)
		}
	})
	t.Run("a tie: both named, refused as the profile", func(t *testing.T) {
		got := callComponentCap(t, profileCeiling(autonomyRubric(types.AutonomyL1)), selfComponent(types.AutonomyL1), exec)
		if got.ok || got.body.Reason != "governance_profile" {
			t.Fatalf("got ok=%v reason %q, want refused as governance_profile", got.ok, got.body.Reason)
		}
		const want = "`task_mode=exec` is not allowed by your governance profile \"autonomy\" at this run's posture: it permits autonomy level L1 " +
			"(bound by egress_sealed, secrets_none, confinement_cc2 and custom_component), and an exec run carries no agent and no tool approvals, " +
			"so nothing supervises it. Launch with an agent instead."
		if got.body.Error != want {
			t.Errorf("sentence =\n  %q\nwant\n  %q", got.body.Error, want)
		}
		if got.body.Policy == nil || got.body.Policy.Source != policyref.SourceProfile {
			t.Errorf("policy = %+v, want the profile", got.body.Policy)
		}
		ok := callComponentCap(t, profileCeiling(autonomyRubric(types.AutonomyL1)), selfComponent(types.AutonomyL1), auto)
		if !slices.Equal(ok.res.BoundBy, append(slices.Clone(rubricCauses), "custom_component")) {
			t.Errorf("bound_by = %v, want the rubric's causes then custom_component", ok.res.BoundBy)
		}
	})
	t.Run("the rubric is lower: the cap is not named", func(t *testing.T) {
		ceiling := profileCeiling(autonomyRubric(types.AutonomyL0))
		want := callComponentCap(t, ceiling, runComponents{}, auto)
		got := callComponentCap(t, ceiling, selfComponent(types.AutonomyL1), auto)
		sameOutcome(t, got, want)
		if got.body.Reason != "governance_profile" || !slices.Equal(got.res.BoundBy, rubricCauses) {
			t.Errorf("got reason %q bound_by %v, want the profile's refusal unchanged", got.body.Reason, got.res.BoundBy)
		}
	})
}
