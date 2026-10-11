// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const previewDoor, preflightDoor = policyPreviewPath, "/api/v1/runs/preflight"

func provRows(t *testing.T, w *httptest.ResponseRecorder) []provenanceRow {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Provenance []provenanceRow `json:"provenance"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Provenance == nil {
		t.Fatalf("provenance missing or null (%v): %s", err, w.Body.String())
	}
	return body.Provenance
}

func provHas(rows []provenanceRow, field, value string, src provSource, effect provEffect) bool {
	return slices.Contains(rows, provenanceRow{Field: field, Value: value, Source: src, Effect: effect})
}

// sameProvenance is the A-S1 promise: the preview and Review answer the same
// request with the same rows.
func sameProvenance(t *testing.T, preview, preflight *httptest.ResponseRecorder) []provenanceRow {
	t.Helper()
	a, b := provRows(t, preview), provRows(t, preflight)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("preview and preflight disagree:\npreview   %+v\npreflight %+v", a, b)
	}
	var sdk client.PreflightResult
	if err := json.Unmarshal(preflight.Body.Bytes(), &sdk); err != nil {
		t.Fatalf("SDK preflight decode: %v", err)
	}
	wantWire, _ := json.Marshal(b)
	gotWire, _ := json.Marshal(sdk.Provenance)
	if string(gotWire) != string(wantWire) {
		t.Fatalf("SDK lost preflight provenance: got %s, want %s", gotWire, wantWire)
	}
	return a
}

func TestProvenanceRecorder_WireShapeAndOrder(t *testing.T) {
	var empty foldRecorder
	if b, _ := json.Marshal(empty.result()); string(b) != "[]" {
		t.Fatalf("an empty recorder marshals %s, want []", b)
	}
	ws := provSource{Kind: provKindWorkspace, ID: "w1", Name: "payments"}
	rec := &foldRecorder{}
	rec.add(fieldGrants, "github_token:acme/one", ws, provEffectAdded)
	rec.add(fieldAllowed, "b.example", provSource{Kind: provKindPolicy, Name: "default"}, provEffectAdded)
	rec.add(fieldAllowed, "a.example", ws, provEffectAdded)
	rec.add(fieldAllowed, "a.example", ws, provEffectAdded)
	rec.add(fieldADOCaps, "pr", provSource{Kind: provKindCeiling}, provEffectNarrowed)
	rec.add(fieldMounts, "/home/agent/work", provSource{Kind: provKindPerson}, provEffectRemoved)
	rec.add(fieldAllowed, "c.example", provSource{Kind: provKindModelProvider, Name: "Corp"}, provEffectClamped)
	got, _ := json.Marshal(rec.result())
	const want = `[` +
		`{"field":"allowed_domains","value":"a.example","source":{"kind":"workspace","id":"w1","name":"payments"},"effect":"added"},` +
		`{"field":"allowed_domains","value":"b.example","source":{"kind":"policy","name":"default"},"effect":"added"},` +
		`{"field":"allowed_domains","value":"c.example","source":{"kind":"model_provider","name":"Corp"},"effect":"clamped"},` +
		`{"field":"eligible_grants","value":"github_token:acme/one","source":{"kind":"workspace","id":"w1","name":"payments"},"effect":"added"},` +
		`{"field":"workspace_mounts","value":"/home/agent/work","source":{"kind":"person"},"effect":"removed"},` +
		`{"field":"azure_devops_capabilities","value":"pr","source":{"kind":"ceiling"},"effect":"narrowed"}]`
	if string(got) != want {
		t.Fatalf("rows =\n%s\nwant\n%s", got, want)
	}
}

func TestRecordPolicyFold(t *testing.T) {
	policy := provSource{Kind: provKindPolicy, Name: "custom"}
	ceiling := provSource{Kind: provKindCeiling}
	authored := types.RunPolicySpec{AllowedDomains: []string{"keep.example", "gone.example"}, MinConfinementClass: types.CC1}
	resolved := types.RunPolicySpec{AllowedDomains: []string{"keep.example"}, DeniedDomains: []string{"deny.example"}, MinConfinementClass: types.CC2}

	rec := &foldRecorder{}
	recordPolicyFold(rec, authored, resolved, policy, true)
	rows := rec.result()
	for _, w := range []provenanceRow{
		{fieldAllowed, "keep.example", policy, provEffectAdded},
		{fieldAllowed, "gone.example", policy, provEffectClamped},
		{fieldDenied, "deny.example", ceiling, provEffectAdded},
		{fieldMinCC, "CC1", policy, provEffectClamped},
		{fieldMinCC, "CC2", ceiling, provEffectAdded},
	} {
		if !slices.Contains(rows, w) {
			t.Errorf("missing %+v in %+v", w, rows)
		}
	}
	for _, r := range rows {
		if r.Field == "first_use_approval" && r.Source.Kind == provKindCeiling {
			t.Errorf("an unset first_use_approval is not the ceiling's: %+v", r)
		}
	}

	// A saved or default policy is the operator's: what the ceiling took out of it is not named.
	rec = &foldRecorder{}
	recordPolicyFold(rec, authored, resolved, provSource{Kind: provKindPolicy, Name: "default"}, false)
	for _, r := range rec.result() {
		if r.Effect == provEffectClamped {
			t.Errorf("a clamped row for a policy the caller did not author: %+v", r)
		}
	}
}

func TestPolicyProvSource(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		rec  policySourceRecord
		want provSource
	}{
		{policySourceRecord{Kind: policyKindStored, PolicyID: &id, Name: "ci"}, provSource{Kind: provKindPolicy, ID: id.String(), Name: "ci"}},
		{policySourceRecord{Kind: policyKindDefault}, provSource{Kind: provKindPolicy, Name: "default"}},
		{policySourceRecord{Kind: policyKindInline}, provSource{Kind: provKindPolicy, Name: "custom"}},
		{policySourceRecord{Kind: policyKindProfile, Name: "walled"}, provSource{Kind: provKindPolicy, Name: "walled"}},
	} {
		if got := policyProvSource(tc.rec); got != tc.want {
			t.Errorf("%+v -> %+v, want %+v", tc.rec, got, tc.want)
		}
	}
}

// the policy chokepoint at both dry doors: policy, ceiling, clamped
func TestProvenance_PolicyCeilingAndClampAtTheDoors(t *testing.T) {
	cs := &capStore{}
	cs.govProfile = govProfile("walled")
	cs.govTier, cs.govHasGroupTier = types.CapabilitySubjectGroup, true
	srv, _, _ := govEscapeFixture(t, cs)
	cookie := govSession(t, govMemberSub, []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com","pastebin.com"]}}`
	rows := sameProvenance(t, doSSO(t, srv, http.MethodPost, previewDoor, cookie, body), doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body))

	custom, ceiling := provSource{Kind: provKindPolicy, Name: "custom"}, provSource{Kind: provKindCeiling}
	for _, w := range []provenanceRow{
		{fieldAllowed, "api.anthropic.com", custom, provEffectAdded},
		{fieldAllowed, "pastebin.com", custom, provEffectClamped},
		{fieldDenied, "corp.internal", ceiling, provEffectAdded},
		{fieldMinCC, "CC1", custom, provEffectClamped},
		{fieldMinCC, "CC2", ceiling, provEffectAdded},
	} {
		if !slices.Contains(rows, w) {
			t.Errorf("missing %+v in %+v", w, rows)
		}
	}
}

// A saved policy the caller did not write keeps its clamped names to itself.
func TestProvenance_SavedPolicyNamesNothingTheCeilingRemoved(t *testing.T) {
	cs := &capStore{}
	cs.govProfile = govProfile("walled")
	cs.govTier, cs.govHasGroupTier = types.CapabilitySubjectGroup, true
	srv, st, _ := govEscapeFixture(t, cs)
	id := uuid.New()
	st.policies[id] = types.RunPolicy{ID: id, Name: "ops", Spec: types.RunPolicySpec{
		MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com", "operator-only.example"}}}
	cookie := govSession(t, govMemberSub, []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `"}`
	rows := sameProvenance(t, doSSO(t, srv, http.MethodPost, previewDoor, cookie, body), doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body))
	saved := provSource{Kind: provKindPolicy, ID: id.String(), Name: "ops"}
	if !provHas(rows, fieldAllowed, "api.anthropic.com", saved, provEffectAdded) {
		t.Errorf("the saved policy's surviving host is not attributed to it: %+v", rows)
	}
	for _, r := range rows {
		if r.Value == "operator-only.example" || r.Effect == provEffectClamped {
			t.Errorf("row discloses what the ceiling removed from a policy the caller did not write: %+v", r)
		}
	}
}

// narrowed, source ceiling: the member's Azure DevOps standing drops a capability
func TestProvenance_ADOStandingNarrowsAtTheDoors(t *testing.T) {
	cs := &capStore{userTypes: utKnown}
	srv, st, _ := govEscapeFixture(t, cs)
	st.siteConfig = adoSite(adoEntraTestRow())
	st.workspaces = []types.Workspace{{
		ID: uuid.New(), Name: "app", OwnedBy: govMemberSub,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}},
	}}
	cookie := govSession(t, govMemberSub, []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
		`"workspace_repos":[{"repo":` + quote(adoTestRepo) + `,"target":"/work/repo"}],"azure_devops_capabilities":["code_read","pr"]}}`
	rows := sameProvenance(t, doSSO(t, srv, http.MethodPost, previewDoor, cookie, body), doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body))
	if !provHas(rows, fieldADOCaps, string(adoscope.CapPR), provSource{Kind: provKindCeiling}, provEffectNarrowed) {
		t.Errorf("the dropped capability is not narrowed by the ceiling: %+v", rows)
	}
	if slices.ContainsFunc(rows, func(r provenanceRow) bool {
		return r.Field == fieldADOCaps && r.Value == string(adoscope.CapCodeRead) && r.Effect == provEffectNarrowed
	}) {
		t.Errorf("a capability the member keeps is narrowed: %+v", rows)
	}
}

// workspace: a requirement's egress host, and the repository the workspace supplies
func TestProvenance_WorkspaceRequirementsAtTheDoors(t *testing.T) {
	srv, st, _ := govEscapeFixture(t, &capStore{})
	ws := types.Workspace{
		ID: uuid.New(), Name: "payments", OwnedBy: govMemberSub, Status: types.WorkspaceScanned,
		Sources:        []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://git.example/acme/pay"}},
		Requirements:   map[string]types.WorkspaceRequirement{"egress:api.stripe.com": {Level: "required", Provenance: "operator_set"}},
		ApprovedEgress: []string{"registry.example"}, DeniedEgress: []string{"bad.example"},
	}
	st.workspaces = []types.Workspace{ws}
	cookie := govSession(t, govMemberSub, []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
		`"workspace_repos":[{"repo":"https://git.example/acme/pay","target":"/work/pay"}]},"workspaces":[{"workspace_id":"` + ws.ID.String() + `"}]}`
	rows := sameProvenance(t, doSSO(t, srv, http.MethodPost, previewDoor, cookie, body), doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body))
	from := provSource{Kind: provKindWorkspace, ID: ws.ID.String(), Name: "payments"}
	if !provHas(rows, fieldAllowed, "api.stripe.com", from, provEffectAdded) {
		t.Errorf("the required egress host is not attributed to its workspace: %+v", rows)
	}
	// The dry doors widen workspace egress themselves (stepPreviewEgress): approved hosts, denies and the clone host.
	for _, w := range []provenanceRow{
		{fieldAllowed, "registry.example", from, provEffectAdded},
		{fieldAllowed, "git.example", from, provEffectAdded},
		{fieldDenied, "bad.example", from, provEffectAdded},
	} {
		if !slices.Contains(rows, w) {
			t.Errorf("missing %+v in %+v", w, rows)
		}
	}
	if !provHas(rows, fieldRepos, "https://git.example/acme/pay", from, provEffectAdded) {
		t.Errorf("the workspace's repository is not attributed to it: %+v", rows)
	}
}

// narrowed, workspace or person: the requirements fold turns a write into a read
func TestRecordRequirementFold_WriteNarrowing(t *testing.T) {
	srv := New(Config{})
	rw := false
	yes := true
	for _, tc := range []struct {
		name     string
		level    string
		sel      client.WorkspaceSelection
		wantKind string
	}{
		{"an optional write nobody enabled is the workspace's", "optional", client.WorkspaceSelection{}, provKindWorkspace},
		{"a required write the person made read-only is the person's", "required", client.WorkspaceSelection{ReadOnly: &yes}, provKindPerson},
		{"a required write nobody narrowed is not narrowed", "required", client.WorkspaceSelection{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := types.Workspace{ID: uuid.New(), Name: "app", Requirements: map[string]types.WorkspaceRequirement{
				"write:/srv/app": {Level: tc.level, Provenance: "operator_set"}}}
			spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/app", Target: "/home/agent/work", ReadOnly: &rw}}}
			before := spec.Clone()
			srv.applyWorkspaceRequirements(t.Context(), &spec, "claude-code", []types.Workspace{ws},
				map[string]client.WorkspaceSelection{ws.ID.String(): tc.sel})
			rec := &foldRecorder{}
			recordRequirementFold(rec, before, spec, ws, tc.sel)
			var narrowed []provenanceRow
			for _, r := range rec.result() {
				if r.Effect == provEffectNarrowed {
					narrowed = append(narrowed, r)
				}
			}
			if tc.wantKind == "" {
				if len(narrowed) != 0 {
					t.Fatalf("narrowed = %+v, want none", narrowed)
				}
				return
			}
			if len(narrowed) != 1 || narrowed[0].Field != fieldMounts || narrowed[0].Value != "/home/agent/work" || narrowed[0].Source.Kind != tc.wantKind {
				t.Fatalf("narrowed = %+v, want the mount narrowed by a %s source", narrowed, tc.wantKind)
			}
		})
	}
}

// component: an organisation's and the person's own, at both doors
func TestProvenance_ComponentsAtTheDoors(t *testing.T) {
	f := newComponentFixture(t)
	orgRef := f.org(compOrgID, types.ComponentDefinition{
		Hosts: []string{"org-api.example"},
		Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}},
	})
	person := map[string]any{"name": "Person Tool", "inline": map[string]any{"hosts": []string{"person-api.example"}}}
	body := componentBody(person, orgRef)
	rows := sameProvenance(t, f.ask(t, previewDoor, body), f.ask(t, preflightDoor, body))
	org := provSource{Kind: provKindComponent, ID: compOrgID, Name: "Org Tool"}
	self := provSource{Kind: provKindComponent, ID: "inline:0", Name: "Person Tool"}
	for _, w := range []provenanceRow{
		{fieldAllowed, "org-api.example", org, provEffectAdded},
		{fieldGrants, "api_key:org-api.example", org, provEffectAdded},
		{fieldAllowed, "person-api.example", self, provEffectAdded},
	} {
		if !slices.Contains(rows, w) {
			t.Errorf("missing %+v in %+v", w, rows)
		}
	}
}

// the direct GitHub clone's hosts, by whoever asked for the clone
func TestProvenance_DirectGitHubEgressAtTheDoors(t *testing.T) {
	member := ssoSession(t, capSub, capEmail, oidc.RoleUser)
	for _, tc := range []struct {
		name, legacy string
		repos        []string
		want         string
	}{
		{"a workspace's repository", "", []string{"acme/one"}, provKindWorkspace},
		{"the request's own repo", "acme/one", nil, provKindPerson},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, req := directGitHubFixture(t, tc.repos...)
			req.Repo = tc.legacy
			raw := string(mustJSON(req))
			rows := sameProvenance(t, doSSO(t, srv, http.MethodPost, previewDoor, member, raw), doSSO(t, srv, http.MethodPost, preflightDoor, member, raw))
			for _, host := range directGitHubHosts {
				i := slices.IndexFunc(rows, func(r provenanceRow) bool {
					return r.Field == fieldAllowed && r.Value == host && r.Source.Kind == tc.want
				})
				if i < 0 {
					t.Errorf("%s: no %s row for %s in %+v", tc.name, tc.want, host, rows)
				}
			}
			if tc.want == provKindWorkspace {
				if r := rows[slices.IndexFunc(rows, func(r provenanceRow) bool { return r.Value == "github.com" && r.Source.Kind == tc.want })]; r.Source.Name != "sources" {
					t.Errorf("workspace source = %+v, want the workspace's name", r.Source)
				}
			}
		})
	}
}

// model_provider: the chosen provider's key host, by name and never by id
func TestProvenance_ModelProviderAtTheDoors(t *testing.T) {
	p := keyProvider("private-provider-sentinel", "claude-code")
	p.Name, p.UID = "Corp Claude", "uid-corp-claude"
	const admin = "sub-admit-admin"
	const body = `{"agent":"claude-code","model_provider":"private-provider-sentinel"}`
	ask := func(path string) *httptest.ResponseRecorder {
		srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(p)}, &capStore{}, nil)
		_ = srv.cfg.Secrets.(*memSecrets).For(admin).Put(t.Context(), providerSecretName(p.UID, providerKeyPart), []byte("sk-owner"))
		return do(t, srv, http.MethodPost, path, providerAdminToken(srv, admin), body)
	}
	rows := sameProvenance(t, ask(previewDoor), ask(preflightDoor))
	if !provHas(rows, fieldAllowed, "api.anthropic.com", provSource{Kind: provKindModelProvider, Name: "Corp Claude"}, provEffectAdded) {
		t.Errorf("the provider's host is not attributed to it by name: %+v", rows)
	}
	for _, r := range rows {
		if r.Source.ID == p.ID || r.Source.ID == p.UID {
			t.Errorf("a provider id reaches the wire: %+v", r)
		}
	}
}

// The wire carries [] for a request with nothing to say, and the field is on both doors.
func TestProvenance_NeverNullAtTheDoors(t *testing.T) {
	srv, _, _ := govEscapeFixture(t, &capStore{})
	cookie := govSession(t, govMemberSub, []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"t"}`
	for _, door := range []string{previewDoor, preflightDoor} {
		w := doSSO(t, srv, http.MethodPost, door, cookie, body)
		provRows(t, w)
	}
}

// Run detail agrees with the doors on whose entry a component's host and a
// direct GitHub clone's hosts are: collectEvidence reads their run.egress.add rows.
func TestRunPolicyView_ComponentAndDirectGitHubEgress(t *testing.T) {
	org := runComponents{attached: []attachedComponent{{
		source: componentSourceOrg, snapshot: types.RunComponent{Name: "Org Tool"}, addedHosts: []string{"org-api.example"},
	}}}
	self := runComponents{attached: []attachedComponent{{
		source: componentSourceSelf, snapshot: types.RunComponent{SelfDefined: true, Name: "Mine"}, addedHosts: []string{"mine.example"},
	}}}
	rows := []pvRow{
		{action: "run.egress.add", data: map[string]any{"kind": "github_direct", "added_domains": []string{"github.com", "*.githubusercontent.com"}}},
		{action: "run.egress.add", data: org.egressAudit()[0]},
		{action: "run.egress.add", data: self.egressAudit()[0]},
	}
	resolved := types.RunPolicySpec{MinConfinementClass: types.CC2,
		AllowedDomains: []string{"github.com", "*.githubusercontent.com", "org-api.example", "mine.example"}}
	base := types.RunPolicySpec{MinConfinementClass: types.CC2}

	t.Run("a run that recorded its starting policy", func(t *testing.T) {
		srv, st := pvFixture(t, &capStore{})
		run := st.seedRun(pvOwner, types.RunRunning)
		st.rows(run.ID, append([]pvRow{pvCreate(nil, pvSource(policyKindInline, nil, "", false, base))}, append(rows, pvResolve(resolved, false))...)...)
		_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
		if c := pvChange(resp.Changes, causeSourceControl, fieldAllowed); c == nil || !slices.Equal(c.Added, []string{"github.com", "*.githubusercontent.com"}) {
			t.Errorf("direct GitHub hosts: %+v in %+v", c, resp.Changes)
		}
		if c := pvChange(resp.Changes, causeComponent, fieldAllowed); c == nil || !slices.Equal(c.Added, []string{"org-api.example"}) {
			t.Errorf("the organisation component's host: %+v in %+v", c, resp.Changes)
		}
		// A person's own component records a count, never its hosts, so its host stays unexplained.
		if c := pvChange(resp.Changes, causeLaunch, fieldAllowed); c == nil || !slices.Equal(c.Added, []string{"mine.example"}) {
			t.Errorf("a person's component host: %+v in %+v", c, resp.Changes)
		}
	})
	t.Run("an older run with no recorded starting policy", func(t *testing.T) {
		srv, st := pvFixture(t, &capStore{})
		run := st.seedRun(pvOwner, types.RunRunning)
		st.rows(run.ID, append(rows, pvResolve(resolved, false))...)
		_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
		if c := pvChange(resp.Changes, causeSourceControl, fieldAllowed); c == nil || !slices.Equal(c.Added, []string{"github.com", "*.githubusercontent.com"}) {
			t.Errorf("direct GitHub hosts: %+v in %+v", c, resp.Changes)
		}
		if c := pvChange(resp.Changes, causeComponent, fieldAllowed); c == nil || !slices.Equal(c.Added, []string{"org-api.example"}) {
			t.Errorf("the organisation component's host: %+v in %+v", c, resp.Changes)
		}
	})
}
