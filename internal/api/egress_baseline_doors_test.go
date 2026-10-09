// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviewAnswer is the slice of POST /runs/preflight these tests read.
type reviewAnswer struct {
	Enforced types.ConfinementClass `json:"enforced_confinement_class"`
	Risk     []composer.RiskItem    `json:"risk_assessment"`
	Autonomy map[string]any         `json:"autonomy"`
}

func (a reviewAnswer) level(field string) composer.RiskLevel {
	for _, it := range a.Risk {
		if it.Field == field {
			return it.Level
		}
	}
	return ""
}

func (a reviewAnswer) posture(axis string) string {
	p, _ := a.Autonomy["posture"].(map[string]any)
	s, _ := p[axis].(string)
	return s
}

func askReview(t *testing.T, srv *Server, body string) reviewAnswer {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", govSession(t, "sub-autonomy", []string{"eng"}, false), body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d, want 200: %s", w.Code, w.Body.String())
	}
	var a reviewAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	return a
}

// A workspace's approved host is declared baseline: Review and launch grade the egress sealed and the
// risk item low. Both doors read the baseline once and pass it to every grader, so a door that grades
// on the built-in set alone reds here, naming which one.
func TestEgressBaselineGradesEgressAtBothDoors(t *testing.T) {
	const host = "forge.corp.example"
	body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],"workspace_repos":[{"repo":"` + govWorkspaceRepo + `"}]}}`
	profile := govProfile("baseline-egress")
	profile.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{EgressOpen: types.AutonomyL1, EgressSealed: types.AutonomyL3}}
	workspaces := []types.Workspace{{
		ID: uuid.New(), Name: "hello",
		Sources:        []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}},
		ApprovedEgress: []string{host},
	}}
	for _, tc := range []struct {
		name        string
		site        types.SiteConfig
		wantEgress  string
		wantLevel   types.AutonomyLevel
		wantAllowed composer.RiskLevel
	}{
		{"nothing declared: the internal host is beyond baseline", types.SiteConfig{}, "open", types.AutonomyL1, composer.RiskMedium},
		{"an internal host not marked changes nothing", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example"}}}, "open", types.AutonomyL1, composer.RiskMedium},
		{"declared exact host", types.SiteConfig{Egress: &types.SiteEgress{BaselineHosts: []string{host}}}, "sealed", types.AutonomyL3, composer.RiskLow},
		{"marked internal host", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}}}, "sealed", types.AutonomyL3, composer.RiskLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, autonomyCapStore(profile))
			st.workspaces, st.siteConfig = workspaces, tc.site
			review := askReview(t, srv, body)

			srv2, st2, audit2 := govEscapeFixture(t, autonomyCapStore(profile))
			st2.workspaces, st2.siteConfig = workspaces, tc.site
			if c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", govSession(t, "sub-autonomy", []string{"eng"}, false), body); c.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
			}
			launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)
			launch := reviewAnswer{Autonomy: launched}

			for _, side := range []struct {
				name string
				got  reviewAnswer
			}{{"review", review}, {"launch", launch}} {
				if side.got.Autonomy == nil {
					t.Fatalf("%s published no autonomy object", side.name)
				}
				if got := side.got.posture("egress"); got != tc.wantEgress {
					t.Errorf("%s: posture.egress = %q, want %q", side.name, got, tc.wantEgress)
				}
				if got, _ := side.got.Autonomy["level"].(string); got != string(tc.wantLevel) {
					t.Errorf("%s: level = %q, want %q", side.name, got, tc.wantLevel)
				}
			}
			if got := review.level("allowed_domains"); got != tc.wantAllowed {
				t.Errorf("review: allowed_domains graded %q, want %q", got, tc.wantAllowed)
			}
		})
	}
}

// An api_key to a declared host is not the confinement floor's business, and grades baseline on the
// secrets axis. Review, launch and the floor all read the one baseline.
func TestEgressBaselineGradesAnAPIKeyAtBothDoors(t *testing.T) {
	grant := types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{
		"host": "llm.corp.example", "header": "Authorization", "secret_name": govCorpSecret,
	})}
	profile := govProfile("baseline-api-key")
	profile.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{SecretsPowerful: types.AutonomyL1, SecretsBaseline: types.AutonomyL3}}
	profile.Ceiling.EligibleGrants = []types.GrantSpec{grant}
	body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com","llm.corp.example"],"eligible_grants":[` + string(mustJSON(grant)) + `]}}`
	fixture := func(site types.SiteConfig) (*Server, *govEscapeStore, *recRecorder) {
		srv, st, audit := govEscapeFixture(t, autonomyCapStore(profile))
		srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{grant}
		st.siteConfig = site
		return srv, st, audit
	}
	declared := types.SiteConfig{Egress: &types.SiteEgress{BaselineHosts: []string{"llm.corp.example"}}}

	srv, _, _ := fixture(types.SiteConfig{})
	if a := askReview(t, srv, body); a.Enforced != types.CC3 || a.posture("secrets") != "powerful" {
		t.Errorf("undeclared: enforced %s, secrets %q; want the CC3 floor and powerful", a.Enforced, a.posture("secrets"))
	}

	srv, _, _ = fixture(declared)
	review := askReview(t, srv, body)
	if review.Enforced != types.CC2 || review.posture("secrets") != "baseline" {
		t.Errorf("declared: review enforced %s, secrets %q; want no floor and baseline", review.Enforced, review.posture("secrets"))
	}
	srv2, st2, audit2 := fixture(declared)
	if c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", govSession(t, "sub-autonomy", []string{"eng"}, false), body); c.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
	}
	launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)
	// The launch posture records the class the run was enforced at: no floor on a declared host.
	launch := reviewAnswer{Autonomy: launched}
	if launch.posture("secrets") != "baseline" || launch.posture("confinement") != "CC2" {
		t.Errorf("launch: secrets %q, confinement %q; want baseline and CC2", launch.posture("secrets"), launch.posture("confinement"))
	}
}

// The facts a run with no repository gets are graded on the same baseline as its enforced class: the
// vault floor on a component's credential to a declared host is lifted at both dry doors, where a
// door that derived its baseline from the SCM-lane read (empty without a repo) would still show it.
func TestEgressBaselineLiftsTheComponentVaultFloorAtBothDryDoors(t *testing.T) {
	header := inlineComponent([]string{"person-api.example"}, headerSecret(compOwnSecret, "person-api.example"))
	for _, tc := range []struct {
		name string
		site *types.SiteEgress
		want bool
	}{
		{"nothing declared", nil, true},
		{"declared host", &types.SiteEgress{BaselineHosts: []string{"person-api.example"}}, false},
	} {
		for _, door := range dryDoors {
			t.Run(tc.name+" "+door, func(t *testing.T) {
				f := newComponentFixture(t)
				f.st.siteConfig.Components = &types.ComponentSettings{RequireVaultForCredentials: true}
				f.st.siteConfig.Egress = tc.site
				facts := doorFacts(t, door, f.ask(t, door, componentBody(header)))
				if len(facts) != 1 {
					t.Fatalf("components = %v, want one", facts)
				}
				var got struct {
					VaultFloor bool `json:"vault_floor"`
				}
				if err := json.Unmarshal([]byte(facts[0]), &got); err != nil || got.VaultFloor != tc.want {
					t.Errorf("vault_floor = %v (%v), want %v: %s", got.VaultFloor, err, tc.want, facts[0])
				}
			})
		}
	}
}

// A synthesized profile is graded on the same baseline: a recording of an internal-only run does not
// grade its allowlist beyond baseline once the operator declared the host.
func TestEgressBaselineGradesASynthesizedProfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		site types.SiteConfig
		want composer.RiskLevel
	}{
		{"nothing declared", types.SiteConfig{}, composer.RiskMedium},
		{"declared host", types.SiteConfig{Egress: &types.SiteEgress{BaselineHosts: []string{"llm.corp.example"}}}, composer.RiskLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			fake := &recordStore{
				run: types.AgentRun{ID: runID, Agent: "claude-code", Repo: "org/repo",
					State: types.RunCompleted, ConfinementClass: types.CC2},
				events: []types.AuditEvent{egressAllowEvent(runID, "llm.corp.example")},
				site:   tc.site,
			}
			cfg := baseTestConfig(h, fake)
			cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"llm.corp.example"}, MinConfinementClass: types.CC2}
			w := do(t, New(cfg), http.MethodPost, "/api/v1/runs/"+runID.String()+"/profile", adminToken, "")
			var resp profileResponse
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
				t.Fatalf("POST .../profile = %d %s", w.Code, w.Body)
			}
			got := reviewAnswer{Risk: resp.RiskAssessment}.level("allowed_domains")
			if got != tc.want {
				t.Errorf("allowed_domains graded %q, want %q (items %+v)", got, tc.want, resp.RiskAssessment)
			}
		})
	}
}
