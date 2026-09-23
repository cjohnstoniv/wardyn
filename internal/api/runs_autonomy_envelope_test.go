// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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

// autonomyDispatchedSpec returns the run.policy.resolve envelope of the one
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
	// Dispatch runs after the 201 (runs_create_launch.go): wait for its envelope.
	ev := waitForRecAudit(t, audit, runID, "run.policy.resolve", "success")
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
			if w.Code == http.StatusCreated {
				fr.waitForSandbox(t) // dispatch runs after the 201
			}
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

// TestAutonomySiteConfigIsReadOnceForGateAndUnion pins that the level and the
// SCM hosts dispatched come from ONE site-config read. With two, a dropped
// connection at the gate or an admin's edit between the reads makes them
// disagree the dangerous way round: graded `sealed` on what the gate saw,
// dispatched to the forge a later read saw.
//
// CreateRun is the store call between the gate and launch's egress union, so
// the first row changes what site config answers there. The second row drops
// the snapshot's own read and nothing else: with the snapshot shared that
// cannot put the two out of step, but a posture graded on a site config
// nobody could read is still a guess, on Review and at launch alike.
func TestAutonomySiteConfigIsReadOnceForGateAndUnion(t *testing.T) {
	const ghes = "ghes.corp.example"
	grant := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: mustJSON(map[string]any{"repos": []string{"acme/widgets"}})}
	body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],"eligible_grants":[` + string(mustJSON(grant)) + `]}}`
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-autonomy", []string{"eng"}, false) }
	fixture := func(t *testing.T) (*Server, *govEscapeStore, *recRecorder) {
		p := govProfile("site-config-once")
		p.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
			EgressOpen: types.AutonomyL1, EgressSealed: types.AutonomyL3,
		}}
		p.Ceiling.EligibleGrants = []types.GrantSpec{grant}
		srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
		srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{grant}
		return srv, st, audit
	}

	t.Run("an scm_hosts edit after the gate does not reach the run", func(t *testing.T) {
		srv, st, audit := fixture(t)
		st.onCreateRun = func() { st.siteConfig = types.SiteConfig{ScmHosts: []string{ghes}} }
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member(t), body); w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
		data := autonomyCreateAudit(t, st, audit)
		eff := autonomyDispatchedSpec(t, st, audit)
		if slices.Contains(eff.AllowedDomains, ghes) && data["tool_approvals"] != "hold" {
			t.Errorf("graded %v, dispatched %v: the SCM host came from a read the gate never saw", data["autonomy"], eff.AllowedDomains)
		}
	})

	t.Run("a failed read refuses the run at both doors", func(t *testing.T) {
		for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
			srv, st, _ := fixture(t)
			// Every other read this request makes succeeds and names the forge;
			// only the snapshot's read drops.
			st.siteConfig = types.SiteConfig{ScmHosts: []string{ghes}}
			st.failSiteConfigReadFrom = "scmLaneSiteConfig"
			w := doSSO(t, srv, http.MethodPost, path, member(t), body)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("%s = %d, want 500 — a posture graded on a site config nobody could read: %s", path, w.Code, w.Body.String())
			}
			st.mu.Lock()
			runs := len(st.runs)
			st.mu.Unlock()
			if runs != 0 {
				t.Errorf("%s left %d run row(s) behind", path, runs)
			}
		}
	})
}

// TestAutonomyADOEntraLaneIsAuthoredOnlyAsGraded is the same one-read property
// for the lane that carries a CREDENTIAL rather than only reach, on the same
// CreateRun span as the SCM test above (#474).
//
// The gate grades the per-person Azure DevOps lane from the site config it read
// at create; dispatch re-reads (siteConfigForDispatch) and authors the
// credential from that SECOND read. So an admin who flips the provider row
// between the two — `shared` to `per_user`, or adding the entra lane — used to
// hand a run graded `secrets=none` the person's Entra bearer, on a rubric that
// caps a powerful secret at a lower rung. The window is not a scheduling race:
// it is the image resolve and the devcontainer/BYOI build inside
// finishCreateRunLaunch, minutes to half an hour.
//
// The assertion is on what dispatch HANDED THE RUNNER, never on the posture:
// both doors agreed on the understated grade, so only the injection rules on
// the sandbox spec can tell the escape from a correct run.
//
// The three rows are the three directions the configuration can move, and they
// are deliberately not one rule:
//
//   - TOWARD the credential — refused. A run whose level was frozen without
//     this credential may not be handed it.
//   - AWAY from it — allowed, and the run still launches. It is capped as
//     though it held a credential it now does not, which is stricter than its
//     posture and harms nobody; refusing would turn a benign admin edit into a
//     failed run.
//   - SIDEWAYS, to another provider row — refused. An Entra access token
//     carries no organisation claim, so the row and organisation pin are the
//     only things scoping the credential, and they may not be re-decided after
//     the level was graded.
func TestAutonomyADOEntraLaneIsAuthoredOnlyAsGraded(t *testing.T) {
	shared := adoEntraTestRow()
	shared.CredentialSource = types.CredentialSourceShared
	otherRow := adoEntraTestRow()
	otherRow.ID = "ado-row-2"

	for _, tc := range []struct {
		name         string
		atTheGate    types.SiteConfig
		afterTheGate types.SiteConfig
		wantRefused  bool
	}{
		{
			name:         "a row flipped to per_user after the gate never reaches the run",
			atTheGate:    adoSite(shared),
			afterTheGate: adoSite(adoEntraTestRow()),
			wantRefused:  true,
		},
		{
			name:         "a row flipped to shared after the gate only caps the run",
			atTheGate:    adoSite(adoEntraTestRow()),
			afterTheGate: adoSite(shared),
			wantRefused:  false,
		},
		{
			name:         "a different provider row after the gate never reaches the run",
			atTheGate:    adoSite(adoEntraTestRow()),
			afterTheGate: adoSite(otherRow),
			wantRefused:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("ado-entra-span")
			p.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
				EgressOpen: types.AutonomyL2, SecretsNone: types.AutonomyL2,
				SecretsBaseline: types.AutonomyL2, SecretsPowerful: types.AutonomyL1,
			}}
			srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
			st.workspaces = []types.Workspace{{ID: uuid.New(), Name: "ado",
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}}}}
			st.siteConfig = tc.atTheGate
			after := tc.afterTheGate
			st.onCreateRun = func() { st.siteConfig = after }

			body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
				`"allowed_domains":["api.anthropic.com"],"workspace_repos":[{"repo":"` + adoTestRepo + `"}]}}`
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, adoTestOwner, []string{"eng"}, false), body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
			}
			var created struct {
				ID uuid.UUID `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			graded, _ := autonomyCreateAudit(t, st, audit)["autonomy"].(map[string]any)
			posture, _ := graded["posture"].(map[string]any)
			secrets, _ := posture["secrets"].(string)
			state, hosts := adoDispatchedInjectionHosts(t, srv, created.ID)

			if slices.Contains(hosts, "dev.azure.com") {
				t.Errorf("dispatch authored an api_key injection to dev.azure.com for a run graded secrets=%s "+
					"from a provider row the gate never saw (state %s, hosts %v)", secrets, state, hosts)
			}
			if tc.wantRefused && state != types.RunFailed {
				t.Errorf("run state = %s, want FAILED: dispatch must refuse the drift rather than launch a run "+
					"whose level was graded against different provider configuration", state)
			}
			if !tc.wantRefused && state == types.RunFailed {
				t.Errorf("run state = FAILED: an edit AWAY from the credential only caps the run and must not " +
					"turn a benign admin change into a failed launch")
			}
		})
	}
}

// adoDispatchedInjectionHosts waits for the detached launch to leave
// PENDING/STARTING and returns the settled state plus the hosts dispatch put on
// the runner's proxy injection rules — empty when CreateSandbox was never
// reached. This is the only place the escape is visible: the run row and the
// audit both record the understated grade.
func adoDispatchedInjectionHosts(t *testing.T, srv *Server, runID uuid.UUID) (types.RunState, []string) {
	t.Helper()
	var state types.RunState
	waitFor(t, "run to settle", func() bool {
		got, err := srv.cfg.Store.GetRun(context.Background(), runID)
		if err != nil {
			return false
		}
		state = got.State
		return state != types.RunPending && state != types.RunStarting
	})
	fr, ok := srv.cfg.Runner.(*fakeRunner)
	if !ok {
		t.Fatalf("fixture runner is %T, not *fakeRunner", srv.cfg.Runner)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.createCalls == 0 {
		return state, nil
	}
	var hosts []string
	for _, g := range fr.lastSpec.ProxyConfig.Injection {
		hosts = append(hosts, g.Rule.Host)
	}
	return state, hosts
}
