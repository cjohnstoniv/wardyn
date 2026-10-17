// Copyright 2026 The Wardyn Authors
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

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	ovHostInside, ovHostOutside = "docs.example.com", "pastebin.com"
	ovSecretValue               = "s3cr3t-value-xyz"
)

// ovMemberFixture is a member bound to a ceiling that allows api.anthropic.com
// and docs.example.com, with two stored-secret owners: the member and someone else.
func ovMemberFixture(t *testing.T) (*Server, *pvStore) {
	t.Helper()
	cs := &capStore{}
	cs.govProfile = govProfile("walled")
	cs.govProfile.Ceiling.AllowedDomains = []string{"api.anthropic.com", ovHostInside}
	// composer.Clamp drops a grant of a kind the ceiling lists none of.
	cs.govProfile.Ceiling.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey,
		Scope: mustJSON(map[string]string{"host": "api.anthropic.com", "secret_name": govCorpSecret})}}
	cs.govTier, cs.govHasGroupTier = types.CapabilitySubjectGroup, true
	srv, st := pvFixture(t, cs)
	// A profile's grants are re-intersected with the deployment's, so the pairing is on both.
	srv.cfg.DefaultPolicy.EligibleGrants = cs.govProfile.Ceiling.EligibleGrants
	srv.cfg.Secrets = &memSecrets{
		m: map[string][]byte{govCorpSecret: []byte("operator")},
		owned: map[string]map[string][]byte{
			govMemberSub: {"my-key": []byte(ovSecretValue)},
			"sub-other":  {"theirs": []byte("theirs")},
		},
	}
	return srv, st
}

func ovBody(overrides string) string {
	return `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com"]},"overrides":` + overrides + `}`
}

func ovCookie(t *testing.T) *http.Cookie { return govSession(t, govMemberSub, []string{"eng"}, false) }

// The answer is the same at create, Review and the preview: an added host
// outside the ceiling is clamped (warning, clamped provenance), inside it is kept.
func TestOverrides_AppliedBeforeTheClamp(t *testing.T) {
	srv, st := ovMemberFixture(t)
	cookie := ovCookie(t)
	body := ovBody(`{"agent":{"add_hosts":["` + ovHostInside + `","` + ovHostOutside + `"]}}`)

	prev := doSSO(t, srv, http.MethodPost, previewDoor, cookie, body)
	rows := sameProvenance(t, prev, doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body))
	person := provSource{Kind: provKindPerson}
	if !provHas(rows, fieldAllowed, ovHostInside, person, provEffectAdded) {
		t.Errorf("a host inside the ceiling is not the person's addition: %+v", rows)
	}
	if !provHas(rows, fieldAllowed, ovHostOutside, person, provEffectClamped) {
		t.Errorf("a host outside the ceiling is not shown as clamped: %+v", rows)
	}
	for _, r := range rows {
		if r.Field == fieldAllowed && r.Value == ovHostInside && r.Source.Kind != provKindPerson {
			t.Errorf("the person's host is also claimed by %+v", r)
		}
	}
	var pv policyPreviewResponse
	if err := json.Unmarshal(prev.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pv.Spec.AllowedDomains, ovHostInside) || slices.Contains(pv.Spec.AllowedDomains, ovHostOutside) {
		t.Errorf("preview allowed_domains = %v", pv.Spec.AllowedDomains)
	}

	// Create: the envelope the proxy enforces carries the same answer.
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	runID := ovOnlyRun(t, st)
	ev := waitForRecAudit(t, st.audit, runID, "run.policy.resolve", "success")
	var env types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &env); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env.AllowedDomains, ovHostInside) || slices.Contains(env.AllowedDomains, ovHostOutside) {
		t.Errorf("create envelope allowed_domains = %v", env.AllowedDomains)
	}
}

func ovOnlyRun(t *testing.T, st *pvStore) uuid.UUID {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.runs) != 1 {
		t.Fatalf("want one run, have %d", len(st.runs))
	}
	for id := range st.runs {
		return id
	}
	return uuid.Nil
}

// With no policy at all the run starts from the caller's ceiling, and an added
// host must still meet it: the default is the ceiling plus the edits.
func TestOverrides_DefaultPolicyEditsAreStillClamped(t *testing.T) {
	srv, _ := ovMemberFixture(t)
	body := `{"agent":"claude-code","task":"t","overrides":{"agent":{"add_hosts":["` + ovHostOutside + `"]}}}`
	w := doSSO(t, srv, http.MethodPost, previewDoor, ovCookie(t), body)
	var pv policyPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v: %s", w.Code, err, w.Body.String())
	}
	if slices.Contains(pv.Spec.AllowedDomains, ovHostOutside) {
		t.Errorf("an edit rode the default policy past the ceiling: %v", pv.Spec.AllowedDomains)
	}
}

// A stored row the ceiling does not clamp as a whole still cannot carry the
// person's additions past it.
func TestOverrides_StoredRowAdditionsAreBounded(t *testing.T) {
	srv, st := ovMemberFixture(t)
	st.capStore.govProfile, st.capStore.govHasGroupTier = nil, false // unassigned: the row is not clamped as a whole
	srv.cfg.DefaultPolicy.AllowedDomains = []string{"api.anthropic.com", ovHostInside}
	id := uuid.New()
	st.policies[id] = types.RunPolicy{ID: id, Name: "ops", Spec: types.RunPolicySpec{
		MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com"}}}
	body := `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `","overrides":{"agent":{"add_hosts":["` + ovHostInside + `","` + ovHostOutside + `"]}}}`
	w := doSSO(t, srv, http.MethodPost, previewDoor, ovCookie(t), body)
	var pv policyPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v: %s", w.Code, err, w.Body.String())
	}
	if !slices.Contains(pv.Spec.AllowedDomains, ovHostInside) || slices.Contains(pv.Spec.AllowedDomains, ovHostOutside) {
		t.Errorf("allowed_domains = %v", pv.Spec.AllowedDomains)
	}
}

func TestOverrides_SecretAddUsesOnlyTheCallersOwnSecret(t *testing.T) {
	srv, _ := ovMemberFixture(t)
	cookie := ovCookie(t)
	add := func(name string) string {
		return ovBody(`{"agent":{"add_secrets":[{"secret_name":"` + name + `","host":"api.anthropic.com"}]}}`)
	}
	w := doSSO(t, srv, http.MethodPost, previewDoor, cookie, add("my-key"))
	rows := provRows(t, w)
	if !provHas(rows, fieldGrants, "api_key:api.anthropic.com", provSource{Kind: provKindPerson}, provEffectAdded) {
		t.Errorf("the caller's own secret is not granted by the person: %+v\n%s", rows, w.Body.String())
	}
	for _, other := range []string{"theirs", govCorpSecret, "nobody-has-this"} {
		for _, door := range []string{previewDoor, "/api/v1/runs"} {
			w := doSSO(t, srv, http.MethodPost, door, cookie, add(other))
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOverrideRefused {
				t.Errorf("%s with %q = %d %s, want 422 %s", door, other, w.Code, w.Body.String(), reasonOverrideRefused)
			}
		}
		// Review answers the same names with the step to take, the row a secret the run needs and the
		// person has not stored gets anywhere else, and the same name, held by someone else or nobody,
		// reads the same.
		w := doSSO(t, srv, http.MethodPost, preflightDoor, cookie, add(other))
		var pf preflightResponse
		if err := json.Unmarshal(w.Body.Bytes(), &pf); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s with %q = %d %s, want 200", preflightDoor, other, w.Code, w.Body.String())
		}
		i := slices.IndexFunc(pf.SetupItems, func(it SetupItem) bool { return it.ID == "secret:"+other })
		if i < 0 || pf.SetupItems[i].Status != "missing" || pf.SetupItems[i].Fix == nil || pf.SetupItems[i].Fix.Action != "add_secret" ||
			pf.SetupItems[i].Fix.SecretName != other {
			t.Errorf("%s with %q: setup items = %+v, want a missing secret row with the add_secret fix", preflightDoor, other, pf.SetupItems)
		}
	}
}

// Keeping nothing cannot be said in the spec (an empty list reads as the row's
// default profile at dispatch), so it is refused, never folded into the default.
func TestOverrides_ADOExplicitEmptyNarrowsToNothing(t *testing.T) {
	spec := types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapPR}}
	e := &overrideEdits{}
	if refusal := applyADOOverride(&spec, client.ADOOverrides{}, spec.AzureDevOpsCapabilities, e); refusal == nil || refusal.body.Reason != reasonOverrideInvalid {
		t.Fatalf("an empty set = %+v, want %s", refusal, reasonOverrideInvalid)
	}
	if len(spec.AzureDevOpsCapabilities) != 2 || len(e.rows) != 0 {
		t.Errorf("a refused empty set changed the spec: %v %v", spec.AzureDevOpsCapabilities, e.rows)
	}
	srv, _ := ovMemberFixture(t)
	w := doSSO(t, srv, http.MethodPost, previewDoor, ovCookie(t), ovBody(`{"azure_devops":{"capabilities":[]}}`))
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonOverrideInvalid {
		t.Errorf("empty set at the door = %d %s", w.Code, w.Body.String())
	}
}

func TestOverrides_ADONarrowsToASubsetOnly(t *testing.T) {
	spec := types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapPR}}
	e := &overrideEdits{}
	if refusal := applyADOOverride(&spec, client.ADOOverrides{Capabilities: []string{"code_read"}}, spec.AzureDevOpsCapabilities, e); refusal != nil {
		t.Fatal(refusal.body)
	}
	if !slices.Equal(spec.AzureDevOpsCapabilities, []adoscope.Capability{adoscope.CapCodeRead}) ||
		!slices.Contains(e.rows, provenanceRow{fieldADOCaps, "pr", provSource{Kind: provKindPerson}, provEffectNarrowed}) {
		t.Errorf("caps = %v rows = %+v", spec.AzureDevOpsCapabilities, e.rows)
	}
	if refusal := applyADOOverride(&spec, client.ADOOverrides{Capabilities: []string{"code_write"}}, []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapPR}, e); refusal == nil || refusal.body.Reason != reasonOverrideRefused {
		t.Errorf("a capability outside the source = %+v, want %s", refusal, reasonOverrideRefused)
	}
}

// overrides_diff records the edits that took effect and nothing else: an edit that
// changed nothing (a host already allowed, a removal of what is not there, a hold
// beneath a standing deny, a git_pat narrowing to what it already is) is left out,
// and a request whose every edit changed nothing records no diff at all.
func TestOverrides_DiffRecordsOnlyEditsThatTookEffect(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"a.example"},
		ToolRules:      []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}},
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT, Scope: mustJSON(types.GitPATScope{Host: "git.example.com", SecretName: "pat", Access: types.PATAccessRead})}},
	}
	e := &overrideEdits{}
	applyHostOverrides(&spec, client.AgentOverrides{AddHosts: []string{"A.example", "b.example"}, RemoveHosts: []string{"gone.example"}}, e)
	if refusal := (&Server{}).applySecretOverrides(t.Context(), httptest.NewRequest(http.MethodPost, "/", nil), &spec, client.AgentOverrides{RemoveSecrets: []string{"absent"}}, e); refusal != nil {
		t.Fatal(refusal.body)
	}
	applyToolRuleOverrides(&spec, []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}, e)
	if refusal := applyGitPATOverrides(&spec, []client.GitPATOverride{{Host: "git.example.com", Access: "read"}}, e); refusal != nil {
		t.Fatal(refusal.body)
	}
	if !slices.Equal(e.applied.Agent.AddHosts, []string{"b.example"}) || e.applied.Agent.RemoveHosts != nil || e.applied.Agent.RemoveSecrets != nil ||
		e.applied.Agent.ToolRules != nil || e.applied.GitPAT != nil {
		t.Errorf("applied = %+v, want only b.example", e.applied.Agent)
	}
	// All no-ops: no diff.
	e = &overrideEdits{}
	applyToolRuleOverrides(&spec, []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}, e)
	src := policySourceRecord{}
	e.finish(&src, spec)
	if src.OverridesDiff != nil {
		t.Errorf("a request of no-ops recorded a diff: %+v", src.OverridesDiff)
	}
}

// The apply step narrows and never widens: removed hosts and secrets go, a tool
// rule keeps the stricter effect, a PAT keeps a subset of its repositories.
func TestOverrides_ApplyNarrowsNeverWidens(t *testing.T) {
	pat := func(sc types.GitPATScope) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(sc)}
	}
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"a.example", "b.example"},
		ToolRules:      []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}, {Tool: "Web", Effect: types.ToolDeny}},
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]string{"host": "a.example", "secret_name": "k1"})},
			pat(types.GitPATScope{Host: "git.example.com", SecretName: "pat", Repos: types.PATRepoSet("r1", "r2"), Access: types.PATAccessWrite}),
		},
	}
	e := &overrideEdits{}
	applyHostOverrides(&spec, client.AgentOverrides{RemoveHosts: []string{"B.example"}}, e)
	applyToolRuleOverrides(&spec, []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}, {Tool: "Web", Effect: types.ToolHold}}, e)
	if refusal := applyGitPATOverrides(&spec, []client.GitPATOverride{{Host: "git.example.com", Repos: types.PATRepoSet("r1"), Access: "read"}}, e); refusal != nil {
		t.Fatal(refusal.body)
	}
	if !slices.Equal(spec.AllowedDomains, []string{"a.example"}) {
		t.Errorf("allowed = %v", spec.AllowedDomains)
	}
	if !slices.Equal(spec.ToolRules, []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}, {Tool: "Web", Effect: types.ToolDeny}}) {
		t.Errorf("tool rules = %v", spec.ToolRules)
	}
	sc, _ := types.DecodeGitPATScope(spec.EligibleGrants[1].Scope)
	if sc.Access != types.PATAccessRead || len(*sc.Repos) != 1 {
		t.Errorf("pat scope = %+v", sc)
	}
	if refusal := applyGitPATOverrides(&spec, []client.GitPATOverride{{Host: "git.example.com", Repos: types.PATRepoSet("r9")}}, e); refusal == nil || refusal.body.Reason != reasonOverrideRefused {
		t.Errorf("a repository the grant does not name = %+v", refusal)
	}
	if refusal := applyGitPATOverrides(&spec, []client.GitPATOverride{{Host: "nowhere.example", Access: "read"}}, e); refusal == nil || refusal.body.Reason != reasonOverrideInvalid {
		t.Errorf("a host with no git_pat grant = %+v", refusal)
	}
	// Removing a secret drops every grant that pairs it.
	if refusal := (&Server{}).applySecretOverrides(t.Context(), httptest.NewRequest(http.MethodPost, "/", nil), &spec, client.AgentOverrides{RemoveSecrets: []string{"k1"}}, e); refusal != nil {
		t.Fatal(refusal.body)
	}
	if slices.ContainsFunc(spec.EligibleGrants, func(g types.GrantSpec) bool { return g.Kind == types.GrantAPIKey }) {
		t.Errorf("the removed secret's grant stands: %v", spec.EligibleGrants)
	}
}

func TestOverrides_PolicySourceRecordsTheDiffAndAttribution(t *testing.T) {
	srv, st := ovMemberFixture(t)
	cookie := ovCookie(t)
	body := ovBody(`{"agent":{"add_hosts":["` + ovHostInside + `","` + ovHostOutside + `"],` +
		`"add_secrets":[{"secret_name":"my-key","host":"api.anthropic.com"}]}}`)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	runID := ovOnlyRun(t, st)
	create := waitForRecAudit(t, st.audit, runID, "run.create", "success")
	var row struct {
		Source *policySourceRecord `json:"policy_source"`
	}
	if err := json.Unmarshal(create.Data, &row); err != nil || row.Source == nil || row.Source.OverridesDiff == nil {
		t.Fatalf("run.create has no overrides_diff (%v): %s", err, create.Data)
	}
	diff := row.Source.OverridesDiff
	if !slices.Equal(diff.Agent.AddHosts, []string{ovHostInside}) || len(diff.Agent.AddSecrets) != 1 ||
		diff.Agent.AddSecrets[0].SecretName != "my-key" || !slices.Contains(diff.Clamped, fieldAllowed+" "+ovHostOutside) {
		t.Errorf("diff = %+v", diff)
	}
	if strings.Contains(string(create.Data), ovSecretValue) {
		t.Errorf("the create row carries a secret value: %s", create.Data)
	}

	// Run detail returns the same diff.
	waitForRecAudit(t, st.audit, runID, "run.policy.resolve", "success")
	_, view := pvMember(t, govMemberSub).get(t, srv, runID)
	if view.Source.OverridesDiff == nil || !slices.Equal(view.Source.OverridesDiff.Agent.AddHosts, diff.Agent.AddHosts) {
		t.Errorf("run detail source = %+v", view.Source)
	}

	// The proxy's refusals name the policy as changed.
	runner := srv.cfg.Runner.(*fakeRunner)
	runner.mu.Lock()
	attr := runner.lastSpec.ProxyConfig.Attribution
	runner.mu.Unlock()
	if attr == nil || !attr.Overridden {
		t.Errorf("proxy attribution = %+v, want overridden", attr)
	}

	// A run nobody edited records no diff and no mark.
	plain := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com"]}}`)
	if plain.Code != http.StatusCreated {
		t.Fatalf("plain create = %d: %s", plain.Code, plain.Body.String())
	}
	var plainRun struct {
		ID uuid.UUID `json:"id"`
	}
	_ = json.Unmarshal(plain.Body.Bytes(), &plainRun)
	ev := waitForRecAudit(t, st.audit, plainRun.ID, "run.create", "success")
	var plainRow struct {
		Source *policySourceRecord `json:"policy_source"`
	}
	if err := json.Unmarshal(ev.Data, &plainRow); err != nil || plainRow.Source == nil || plainRow.Source.OverridesDiff != nil {
		t.Errorf("an unedited run records a diff (%v): %s", err, ev.Data)
	}
}

// Run-wide hosts on top of a base: an allowed host is a widening the ceiling
// bounds (inside kept, outside shown as clamped); a denied host only tightens
// and always stands. A base host cannot be removed.
func TestRunHostOverrides_AddAllowedClampedAddDeniedAllowed(t *testing.T) {
	srv, _ := ovMemberFixture(t)
	cookie := ovCookie(t)
	body := ovBody(`{"hosts":{"add_allowed":["` + ovHostInside + `","` + ovHostOutside + `"],"add_denied":["*.tracker.example"]}}`)
	prev := doSSO(t, srv, http.MethodPost, previewDoor, cookie, body)
	rows := sameProvenance(t, prev, doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body))
	person := provSource{Kind: provKindPerson}
	for _, w := range []provenanceRow{
		{fieldAllowed, ovHostInside, person, provEffectAdded},
		{fieldAllowed, ovHostOutside, person, provEffectClamped},
		{fieldDenied, "*.tracker.example", person, provEffectAdded},
	} {
		if !slices.Contains(rows, w) {
			t.Errorf("missing %+v in %+v", w, rows)
		}
	}
	var pv policyPreviewResponse
	if err := json.Unmarshal(prev.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pv.Spec.AllowedDomains, ovHostInside) || slices.Contains(pv.Spec.AllowedDomains, ovHostOutside) ||
		!slices.Contains(pv.Spec.DeniedDomains, "*.tracker.example") || !slices.Contains(pv.Spec.AllowedDomains, "api.anthropic.com") {
		t.Errorf("spec allowed %v denied %v", pv.Spec.AllowedDomains, pv.Spec.DeniedDomains)
	}

	for _, refused := range []string{
		`{"hosts":{"remove_allowed":["api.anthropic.com"]}}`,
		`{"hosts":{"remove_denied":["corp.internal"]}}`,
		`{"hosts":{"add_allowed":["*.example.com"]}}`,
	} {
		for _, door := range []string{previewDoor, preflightDoor, "/api/v1/runs"} {
			w := doSSO(t, srv, http.MethodPost, door, cookie, ovBody(refused))
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOverrideRefused {
				t.Errorf("%s %s = %d %s, want 422 %s", door, refused, w.Code, w.Body.String(), reasonOverrideRefused)
			}
		}
	}
}

// First-use handling and "request all other egress" on a stored base: a value past
// the ceiling is clamped back to the base's, one within it is applied.
func TestRunEgressOverrides_StoredBaseClampedPastTheCeilingAppliedWithin(t *testing.T) {
	srv, st := ovMemberFixture(t)
	st.capStore.govProfile, st.capStore.govHasGroupTier = nil, false // unassigned: the row is not clamped as a whole
	id := uuid.New()
	st.policies[id] = types.RunPolicy{ID: id, Name: "ops", Spec: types.RunPolicySpec{
		MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com"}, FirstUseApproval: types.FirstUseAlwaysDeny}}
	preview := func(ceiling types.RunPolicySpec, egress string) (types.RunPolicySpec, []provenanceRow) {
		t.Helper()
		srv.cfg.DefaultPolicy = ceiling
		body := `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `","overrides":{"egress":` + egress + `}}`
		w := doSSO(t, srv, http.MethodPost, previewDoor, ovCookie(t), body)
		var pv policyPreviewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%d %v: %s", w.Code, err, w.Body.String())
		}
		return pv.Spec, pv.Provenance
	}
	person := provSource{Kind: provKindPerson}
	both := `{"first_use_approval":"wait_for_review","allow_all_egress":true}`

	narrow := govDeployment()
	narrow.FirstUseApproval = types.FirstUseAlwaysDeny
	spec, rows := preview(narrow, both)
	if spec.AllowAllEgress || spec.FirstUseApproval != types.FirstUseAlwaysDeny {
		t.Errorf("past the ceiling: allow_all_egress %v first_use %q, want both held back", spec.AllowAllEgress, spec.FirstUseApproval)
	}
	if !provHas(rows, "allow_all_egress", "true", person, provEffectClamped) || !provHas(rows, "first_use_approval", "wait_for_review", person, provEffectClamped) {
		t.Errorf("clamped edits are not shown as clamped: %+v", rows)
	}

	wide := govDeployment()
	wide.AllowAllEgress, wide.FirstUseApproval = true, types.FirstUseWaitForReview
	spec, rows = preview(wide, both)
	if !spec.AllowAllEgress || spec.FirstUseApproval != types.FirstUseWaitForReview {
		t.Errorf("within the ceiling: allow_all_egress %v first_use %q, want both applied", spec.AllowAllEgress, spec.FirstUseApproval)
	}
	if !provHas(rows, "allow_all_egress", "true", person, provEffectAdded) || !provHas(rows, "first_use_approval", "wait_for_review", person, provEffectAdded) {
		t.Errorf("applied edits are not the person's: %+v", rows)
	}

	for _, bad := range []string{`{"first_use_approval":"sometimes"}`, `{"allow_all_egress":false}`} {
		w := doSSO(t, srv, http.MethodPost, previewDoor, ovCookie(t), `{"agent":"claude-code","task":"t","overrides":{"egress":`+bad+`}}`)
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonOverrideInvalid {
			t.Errorf("%s = %d %s, want 400 %s", bad, w.Code, w.Body.String(), reasonOverrideInvalid)
		}
	}
}
