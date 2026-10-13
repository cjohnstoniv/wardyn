// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A member on the no-policy default who only NARROWS something keeps the
// operator's workspace_mounts: the default IS their ceiling, and clamping it as a
// whole would drop a mount the operator authored. Every door agrees, and an
// addition in the same request is still bounded.
func TestOverrides_NarrowingOnTheDefaultKeepsItsMounts(t *testing.T) {
	ws := &types.Workspace{ID: uuid.New(), Name: "ops", Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/ops", Target: "/home/agent/ops"}}}
	srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, ws)
	srv.cfg.DefaultPolicy.WorkspaceMounts = []types.WorkspaceMount{{Source: "/srv/ops", Target: "/home/agent/ops"}}
	cookie := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	body := `{"agent":"claude-code","task":"t","overrides":{"hosts":{"add_denied":["bad.example"],"add_allowed":["` + ovHostOutside + `"]}}}`

	var pv policyPreviewResponse
	w := doSSO(t, srv, http.MethodPost, previewDoor, cookie, body)
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
		t.Fatalf("preview %d %v: %s", w.Code, err, w.Body.String())
	}
	if len(pv.Spec.WorkspaceMounts) != 1 || pv.Spec.WorkspaceMounts[0].Target != "/home/agent/ops" {
		t.Errorf("preview dropped the operator's mount: %+v", pv.Spec.WorkspaceMounts)
	}
	if !slices.Contains(pv.Spec.DeniedDomains, "bad.example") || slices.Contains(pv.Spec.AllowedDomains, ovHostOutside) {
		t.Errorf("preview denied = %v allowed = %v, want the denial kept and the outside host bounded", pv.Spec.DeniedDomains, pv.Spec.AllowedDomains)
	}
	if w := doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body); w.Code != http.StatusOK {
		t.Fatalf("preflight = %d: %s", w.Code, w.Body.String())
	}
	w = doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create = %d %v: %s", w.Code, err, w.Body.String())
	}
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), created.ID, "run.policy.resolve", "success")
	var env types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.WorkspaceMounts) != 1 || env.WorkspaceMounts[0].Target != "/home/agent/ops" {
		t.Errorf("create's envelope lost the operator's mount: %+v", env.WorkspaceMounts)
	}
}

// The Azure DevOps card narrows a run whose source names no set (every governed
// no-policy run) within the default profile of the provider row that admits its
// repository, the set dispatch gives such a run.
func TestOverrides_ADONarrowsWithinTheRowDefaultProfileOnANoPolicyRun(t *testing.T) {
	site := adoSite(adoEntraTestRow()) // default profile [code_read code_write], ceiling adds pr
	cookie := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	body := func(caps string) string {
		return `{"agent":"claude-code","task":"t","repo":"` + adoTestRepo + `","overrides":{"azure_devops":{"capabilities":[` + caps + `]}}}`
	}
	person := provSource{Kind: provKindPerson}
	for _, door := range []string{previewDoor, preflightDoor} {
		srv := providerRunFixture(t, site, &capStore{}, nil)
		w := doSSO(t, srv, http.MethodPost, door, cookie, body(`"code_read"`))
		if !provHas(provRows(t, w), fieldADOCaps, "code_write", person, provEffectNarrowed) {
			t.Errorf("%s: dropping code_write from the default profile is not the person's narrowing: %s", door, w.Body.String())
		}
		// Naming the whole default is no edit.
		w = doSSO(t, srv, http.MethodPost, door, cookie, body(`"code_read","code_write"`))
		for _, row := range provRows(t, w) {
			if row.Source == person {
				t.Errorf("%s: keeping the whole default recorded %+v", door, row)
			}
		}
	}
	var pv policyPreviewResponse
	srv := providerRunFixture(t, site, &capStore{}, nil)
	if w := doSSO(t, srv, http.MethodPost, previewDoor, cookie, body(`"code_read"`)); json.Unmarshal(w.Body.Bytes(), &pv) != nil ||
		!slices.Equal(pv.Spec.AzureDevOpsCapabilities, []adoscope.Capability{adoscope.CapCodeRead}) {
		t.Errorf("resolved ADO capabilities = %v: %s", pv.Spec.AzureDevOpsCapabilities, w.Body.String())
	}
	// A capability beyond the default is an addition at every door, in the
	// refusal's own words; so is a run whose repository resolves to no row.
	for _, tc := range []struct {
		name, repo string
		site       types.SiteConfig
		caps       string
		want       string
	}{
		{"beyond the default profile", adoTestRepo, site, `"pr"`, "pr is not among the capabilities this run has"},
		{"no per-person Azure DevOps access", "acme/one", types.SiteConfig{}, `"code_read"`, "resolves to no per-person Azure DevOps access"},
	} {
		for _, door := range []string{previewDoor, preflightDoor, "/api/v1/runs"} {
			srv := providerRunFixture(t, tc.site, &capStore{}, nil)
			b := strings.Replace(body(tc.caps), adoTestRepo, tc.repo, 1)
			w := doSSO(t, srv, http.MethodPost, door, cookie, b)
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOverrideRefused || !strings.Contains(w.Body.String(), tc.want) ||
				strings.Contains(w.Body.String(), "cannot add") {
				t.Errorf("%s at %s = %d %s, want 422 %s naming %q", tc.name, door, w.Code, w.Body.String(), reasonOverrideRefused, tc.want)
			}
		}
	}
}

// remove_secrets asks who owns the name before it looks at the policy, and a name
// that is someone else's answers exactly as one nobody has, at every door and in
// every member output. A secret the caller owns still removes the grant that
// pairs it, and an owned name nothing pairs is a plain no-op.
func TestOverrides_RemoveSecretsIsNoOracleForOperatorNames(t *testing.T) {
	srv, _ := ovMemberFixture(t)
	srv.cfg.Secrets.(*memSecrets).owned[govMemberSub]["spare-key"] = []byte("v")
	cookie := ovCookie(t)
	policy := `"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","secret_name":"my-key"}},` +
		`{"kind":"api_key","scope":{"host":"docs.example.com","secret_name":"` + govCorpSecret + `"}}]`
	remove := func(name string) string {
		return `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com"],` + policy +
			`},"overrides":{"agent":{"remove_secrets":["` + name + `"]}}}`
	}

	for _, door := range []string{previewDoor, preflightDoor, "/api/v1/runs"} {
		refusals := map[string]string{}
		for _, name := range []string{govCorpSecret, "theirs", "nobody-has-this"} {
			w := doSSO(t, srv, http.MethodPost, door, cookie, remove(name))
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOverrideRefused {
				t.Fatalf("%s remove %q = %d %s, want 422 %s", door, name, w.Code, w.Body.String(), reasonOverrideRefused)
			}
			refusals[name] = strings.ReplaceAll(w.Body.String(), name, "<name>")
		}
		if refusals[govCorpSecret] != refusals["nobody-has-this"] || refusals["theirs"] != refusals["nobody-has-this"] {
			t.Errorf("%s: the refusal tells an operator name from an unknown one: %v", door, refusals)
		}
	}

	// Controls: the caller's own paired secret removes its grant; the caller's own
	// unpaired secret changes nothing.
	person := provSource{Kind: provKindPerson}
	hit := provRows(t, doSSO(t, srv, http.MethodPost, previewDoor, cookie, remove("my-key")))
	if !provHas(hit, fieldGrants, "api_key:api.anthropic.com", person, provEffectRemoved) {
		t.Errorf("removing the caller's paired secret did not remove its grant: %+v", hit)
	}
	miss := provRows(t, doSSO(t, srv, http.MethodPost, previewDoor, cookie, remove("spare-key")))
	for _, row := range miss {
		if row.Source == person {
			t.Errorf("an owned name nothing pairs recorded %+v", row)
		}
	}
}

// The create door audits a stored row's addition that the enforced egress
// capability drops, once and only that host: the policy's own hosts, which the
// same probe drops from its copy but the run keeps, are not audited, and the
// dry doors stay silent.
func TestOverrides_AuditsOnlyTheAdditionsTheCapabilityDrops(t *testing.T) {
	const baseDenied = "base-denied.example"
	srv, st := ovMemberFixture(t)
	st.capStore.govProfile, st.capStore.govHasGroupTier = nil, false // unassigned: the row is not clamped as a whole
	st.capStore.enf = map[string]bool{capEgressHost: true}
	st.capStore.grants = []types.CapabilityGrant{{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: govMemberSub,
		Capability: capEgressHost, Value: "api.anthropic.com", Effect: types.CapabilityAllow}}
	srv.cfg.DefaultPolicy.AllowedDomains = []string{"api.anthropic.com", ovHostInside, baseDenied}
	id := uuid.New()
	st.policies[id] = types.RunPolicy{ID: id, Name: "ops", Spec: types.RunPolicySpec{
		MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com", baseDenied}}}
	body := `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `","overrides":{"agent":{"add_hosts":["` + ovHostInside + `"]}}}`
	cookie := ovCookie(t)
	drops := func() []string {
		var out []string
		for _, ev := range st.audit.snapshot() {
			if ev.Action == authz.AuditAction && ev.Target == "runs.inline_policy" {
				out = append(out, string(ev.Data))
			}
		}
		return out
	}

	for _, door := range []string{previewDoor, preflightDoor} {
		if w := doSSO(t, srv, http.MethodPost, door, cookie, body); w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", door, w.Code, w.Body.String())
		}
	}
	if got := drops(); len(got) != 0 {
		t.Fatalf("a dry door audited the drop: %v", got)
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	got := drops()
	if len(got) != 1 || !strings.Contains(got[0], ovHostInside) || strings.Contains(got[0], baseDenied) {
		t.Errorf("create audited %v, want one drop naming only %s", got, ovHostInside)
	}
}

// A diff holding only what the ceiling clamped changed nothing, so the run is not
// marked overridden (its refusals would otherwise call the policy the person's).
func TestOverrides_ClampedOnlyDiffIsNotOverridden(t *testing.T) {
	clamped := client.RunOverridesDiff{Clamped: []string{fieldAllowed + " " + ovHostOutside}}
	applied := client.RunOverridesDiff{RunOverrides: client.RunOverrides{Agent: &client.AgentOverrides{AddHosts: []string{ovHostInside}}}}
	mixed := applied
	mixed.Clamped = clamped.Clamped
	for _, tc := range []struct {
		name string
		diff *client.RunOverridesDiff
		want bool
	}{{"no diff", nil, false}, {"clamped only", &clamped, false}, {"applied", &applied, true}, {"applied and clamped", &mixed, true}} {
		if got := (policySourceRecord{OverridesDiff: tc.diff}).overridden(); got != tc.want {
			t.Errorf("%s: overridden = %v, want %v", tc.name, got, tc.want)
		}
	}

	srv, _ := ovMemberFixture(t)
	body := ovBody(`{"agent":{"add_hosts":["` + ovHostOutside + `"]}}`)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", ovCookie(t), body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	runner := srv.cfg.Runner.(*fakeRunner)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if attr := runner.lastSpec.ProxyConfig.Attribution; attr != nil && attr.Overridden {
		t.Errorf("a run whose only edit was clamped is attributed as overridden: %+v", attr)
	}
}

// The git component facts carry the push rules enforcement applies to each
// organisation, a person's keyed rule included, and keep them apart.
func TestComponentFacts_CarryPersonalKeyedPushRulesPerOrganisation(t *testing.T) {
	for _, door := range dryDoors {
		f := newComponentFixture(t)
		row := githubRow("github-row", false, "https://github.com")
		f.st.siteConfig = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}}
		f.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true, PushRules: &types.PushRulesSpec{DenyPaths: []string{"private/**"}}}
		id := uuid.New()
		f.st.ws = &types.Workspace{ID: id, Name: "Approved workspace", Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/one", Target: "/home/agent/one"},
			{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/other/two", Target: "/home/agent/two"},
		}}
		f.st.workspaces = []types.Workspace{*f.st.ws}
		body := `{"agent":"claude-code","task":"facts","workspace_id":"` + id.String() + `","overrides":{"push_rules":[` +
			`{"provider":"github","org":"acme","deny_paths":["acme-only/**"],"require_review_paths":["review/**"]}]}}`
		w := kernelLaunch(t, f.srv, f.st.govEscapeStore, kernelUserTier, []string{"eng"}, door, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", door, w.Code, w.Body.String())
		}
		var response struct {
			Components []client.ComponentFact `json:"components"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		byOrg := map[string]*types.PushRulesSpec{}
		for _, fact := range response.Components {
			if fact.Kind == types.ComponentGitProvider {
				byOrg[fact.Org] = fact.PushRules
			}
		}
		acme, other := byOrg["acme"], byOrg["other"]
		if acme == nil || !slices.Equal(acme.DenyPaths, []string{"private/**", "acme-only/**"}) || !slices.Equal(acme.RequireReviewPaths, []string{"review/**"}) {
			t.Errorf("%s: acme facts omit the person's keyed rule: %+v", door, acme)
		}
		if other == nil || !slices.Equal(other.DenyPaths, []string{"private/**"}) || len(other.RequireReviewPaths) != 0 {
			t.Errorf("%s: another organisation's facts took acme's rule or lost the base: %+v", door, other)
		}
	}
}
