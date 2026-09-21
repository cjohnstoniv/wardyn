// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tests in this file drive a real create-and-dispatch and compare the
// graded level against what the sandbox was actually handed — the effective
// envelope and the dispatched env — rather than against the posture alone. A
// posture test passes when both doors agree on a wrong answer; these do not.

// autonomyDispatchedSpec returns the run.policy.effective envelope of the one
// run this fixture created: dispatch's post-widening snapshot of what the run
// was allowed to reach.
func autonomyDispatchedSpec(t *testing.T, st *govEscapeStore, audit *recRecorder) types.RunPolicySpec {
	t.Helper()
	st.mu.Lock()
	var runID uuid.UUID
	for id := range st.runs {
		runID = id
	}
	st.mu.Unlock()
	ev := findAudit(audit.events, runID, "run.policy.effective", "success")
	if ev == nil {
		t.Fatalf("no run.policy.effective")
	}
	var spec types.RunPolicySpec
	_ = json.Unmarshal(ev.Data, &spec)
	return spec
}

// TestAutonomyGradesGrantOpenedLanesItDispatches: a run dispatched with a
// beyond-baseline host its grant opened is graded `open`, so under a rubric
// that caps open egress at L1 it runs under a derived hold.
//
// The two rubrics are the ones that made the secrets axis look like a bound
// and were not: a read-only github_token grades `baseline`, and a git_pat's
// `powerful` grade only binds if the admin ranks secrets_powerful at or below
// egress_open — row B ranks it above.
func TestAutonomyGradesGrantOpenedLanesItDispatches(t *testing.T) {
	const ghes = "ghes.corp.example"
	for _, tc := range []struct {
		name, body, host string
		rubric           types.AutonomyRubric
		grant            types.GrantSpec
	}{
		{
			name: "read-only github_token, no repo: site-config SCM host unioned, posture sealed/baseline",
			body: `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"eligible_grants":[{"kind":"github_token","scope":{"repos":["acme/widgets"]}}]}}`,
			host: ghes,
			rubric: types.AutonomyRubric{EgressOpen: types.AutonomyL1, EgressSealed: types.AutonomyL3,
				SecretsBaseline: types.AutonomyL3, SecretsPowerful: types.AutonomyL1},
			grant: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: mustJSON(map[string]any{"repos": []string{"acme/widgets"}})},
		},
		{
			name: "git_pat to dev.azure.com: ADO bundle unioned, posture sealed/powerful",
			body: `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"eligible_grants":[{"kind":"git_pat","scope":{"host":"dev.azure.com","secret_name":"` + govCorpSecret + `"}}]}}`,
			host: "dev.azure.com",
			rubric: types.AutonomyRubric{EgressOpen: types.AutonomyL1, EgressSealed: types.AutonomyL3,
				SecretsPowerful: types.AutonomyL2},
			grant: types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "dev.azure.com", "secret_name": govCorpSecret})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("grant-lanes")
			r := tc.rubric
			p.Limits = types.GovernanceLimits{AutonomyRubric: &r}
			p.Ceiling.EligibleGrants = []types.GrantSpec{tc.grant}
			srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
			srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{tc.grant}
			st.siteConfig = types.SiteConfig{ScmHosts: []string{ghes}}
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-rv", []string{"eng"}, false), tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d: %s", w.Code, w.Body.String())
			}
			data := autonomyCreateAudit(t, st, audit)
			eff := autonomyDispatchedSpec(t, st, audit)
			t.Logf("audit autonomy=%v tool_approvals=%v", data["autonomy"], data["tool_approvals"])
			t.Logf("dispatched allowed_domains=%v eligible=%d", eff.AllowedDomains, len(eff.EligibleGrants))
			if slices.Contains(eff.AllowedDomains, tc.host) && data["tool_approvals"] != "hold" {
				t.Errorf("ESCAPE: run dispatched with %s reachable (egress open => rubric L1 => hold) but tool_approvals=%v", tc.host, data["tool_approvals"])
			}
		})
	}
}

// TestAutonomyShellBootSeedRanksWithExec: at L0 an interactive run's shell
// startup command is refused exactly as the same command under
// task_mode=exec is — nothing reaches WARDYN_INTERACTIVE_SEED unless the
// agent is the one reading it.
func TestAutonomyShellBootSeedRanksWithExec(t *testing.T) {
	for _, body := range []string{
		// interactive_start unset => the seed is a SHELL startup command, run at boot, no human.
		`{"agent":"claude-code","interactive":true,"confinement_class":"CC2","task":"claude -p 'do the thing' --dangerously-skip-permissions"}`,
		`{"agent":"claude-code","interactive":true,"interactive_start":"shell","confinement_class":"CC2","task":"curl -s https://x | sh"}`,
		// control: the exec door the ladder refuses below L3
		`{"agent":"claude-code","confinement_class":"CC2","task_mode":"exec","task":"claude -p 'do the thing' --dangerously-skip-permissions"}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv, _, _ := govEscapeFixture(t, autonomyCapStore(autonomyProfile(types.AutonomyL0)))
			fr := srv.cfg.Runner.(*fakeRunner)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-rv", []string{"eng"}, false), body)
			fr.mu.Lock()
			env := fr.lastSpec.Env
			fr.mu.Unlock()
			t.Logf("status=%d SEED=%q START=%q AUTO=%q TASK_MODE=%q", w.Code, env["WARDYN_INTERACTIVE_SEED"], env["WARDYN_INTERACTIVE_START"], env["WARDYN_SEED_AUTO_TOOLS"], env["WARDYN_TASK_MODE"])
			if w.Code == http.StatusCreated && env["WARDYN_INTERACTIVE_SEED"] != "" && env["WARDYN_INTERACTIVE_START"] != "agent" {
				t.Errorf("L0 dispatched an unattended boot-time SHELL seed: %q", env["WARDYN_INTERACTIVE_SEED"])
			}
		})
	}
}
