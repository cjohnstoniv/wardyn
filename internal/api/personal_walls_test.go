// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A person's own walls (personal_walls.go): the /me/walls routes, the run step inside the boundary's
// on every arm, the seal layer, the approval admit and the run.ceiling.reassert row.

const wallsSub = "sub-walls"

var wallsSSH = types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"git.corp.example","key_secret_ref":"deploy-key"}`)}

// wallsStore is integStore plus the principal_prefs table, for a launch through the real doors.
type wallsStore struct {
	*integStore
	prefs map[string]json.RawMessage
}

func (s *wallsStore) GetPrincipalPref(_ context.Context, principal, key string) (json.RawMessage, error) {
	v, ok := s.prefs[principal+"|"+key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (s *wallsStore) PutPrincipalPref(_ context.Context, principal, key string, value json.RawMessage) error {
	s.prefs[principal+"|"+key] = value
	return nil
}

// wallsRunFixture is providerRunFixture over a store that keeps principal preferences.
func wallsRunFixture(t *testing.T) (*Server, *wallsStore) {
	t.Helper()
	h := newHarness(t)
	st := &wallsStore{integStore: &integStore{govEscapeStore: newGovEscapeStore(&capStore{})}, prefs: map[string]json.RawMessage{}}
	cfg := baseTestConfig(h, st)
	cfg.Audit = &recRecorder{}
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), st
}

// saveWalls stores spec as principal's walls in prefs, as PUT /me/walls does.
func saveWalls(t *testing.T, prefs store.PrincipalPrefStore, principal string, spec types.PersonalWallsSpec) {
	t.Helper()
	if err := prefs.PutPrincipalPref(context.Background(), principal, types.PersonalWallsPrefKey, mustJSON(types.PersonalWalls{Spec: spec})); err != nil {
		t.Fatal(err)
	}
}

func wallsList(v ...string) *[]string { return &v }

// wallsAuditRow is the last row of action in events (a walls write carries no run id).
func wallsAuditRow(events []types.AuditEvent, action string) *types.AuditEvent {
	var hit *types.AuditEvent
	for i := range events {
		if events[i].Action == action && events[i].Outcome == "success" {
			hit = &events[i]
		}
	}
	return hit
}

// TestValidatePersonalWalls: walls may only narrow, and each refusal names the first path it refuses.
func TestValidatePersonalWalls(t *testing.T) {
	on, empty, cc9, review := true, types.FirstUseMode("ask_me"), types.ConfinementClass("CC9"), types.FirstUseWaitForReview
	tooMany := make([]string, maxWallsDomains+1)
	for i := range tooMany {
		tooMany[i] = "h" + strings.Repeat("x", i%5) + ".example"
	}
	for _, tc := range []struct {
		name string
		spec types.PersonalWallsSpec
		path string
	}{
		{"an IP address", types.PersonalWallsSpec{AllowedDomains: wallsList("10.0.0.1")}, "allowed_domains[0]"},
		{"a port", types.PersonalWallsSpec{AllowedDomains: wallsList("pypi.org", "pypi.org:443")}, "allowed_domains[1]"},
		{"a URL", types.PersonalWallsSpec{DeniedDomains: wallsList("https://evil.example/")}, "denied_domains[0]"},
		{"over the cap", types.PersonalWallsSpec{DeniedDomains: &tooMany}, "denied_domains:"},
		{"allow-all on", types.PersonalWallsSpec{AllowAllEgress: &on}, "allow_all_egress"},
		{"an unknown first-use mode", types.PersonalWallsSpec{FirstUseApproval: &empty}, "first_use_approval"},
		{"an unknown barrier", types.PersonalWallsSpec{MinConfinementClass: &cc9}, "min_confinement_class"},
		{"a desktop direction allowed", types.PersonalWallsSpec{Desktop: &types.DesktopControls{CopyOut: types.TransferAllowed}}, "desktop.copy_out"},
		{"an unknown grant kind", types.PersonalWallsSpec{WithheldGrants: []types.GrantSpec{{Kind: "nope"}}}, "withheld_grants[0]"},
		{"owner_only_refs authored", types.PersonalWallsSpec{WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey, OwnerOnlyRefs: []string{"x"}}}}, "withheld_grants[0]"},
	} {
		err := validatePersonalWalls(tc.spec)
		if err == nil || !strings.HasPrefix(err.Error(), tc.path) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.path)
		}
	}
	ok := types.PersonalWallsSpec{
		AllowedDomains: wallsList("pypi.org", "*.github.com"), DeniedDomains: wallsList("*.evil.example"),
		FirstUseApproval: &review, Desktop: &types.DesktopControls{PasteIn: types.TransferBlocked},
		WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey}},
	}
	if err := validatePersonalWalls(ok); err != nil {
		t.Errorf("valid walls refused: %v", err)
	}
}

// TestPersonalWallsRoutes: GET, PUT and DELETE act on the signed-in person's own document; a caller
// who is no person is refused, invalid walls name their path, and a store with no preferences is 501.
func TestPersonalWallsRoutes(t *testing.T) {
	srv, st, h := uvServer(t)
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)

	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me/walls", member, ""); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"spec":{}}` {
		t.Fatalf("GET with none = %d %s, want 200 {\"spec\":{}}", w.Code, w.Body)
	}
	w := doSSO(t, srv, http.MethodPut, "/api/v1/me/walls", member, `{"spec":{"allowed_domains":["10.0.0.1"]}}`)
	if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonWallsInvalid || !strings.Contains(w.Body.String(), "allowed_domains[0]") {
		t.Errorf("PUT an IP address = %d %s, want 400 %s naming allowed_domains[0]", w.Code, w.Body, reasonWallsInvalid)
	}
	if _, ok := st.prefs[wallsSub+"|"+types.PersonalWallsPrefKey]; ok {
		t.Fatal("refused walls were stored")
	}
	body := `{"spec":{"allowed_domains":["pypi.org"],"denied_domains":["evil.example"],"withheld_grants":[{"kind":"ssh_key"}]}}`
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/walls", member, body); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	w = doSSO(t, srv, http.MethodGet, "/api/v1/me/walls", member, "")
	var got types.PersonalWalls
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET = %d %v: %s", w.Code, err, w.Body)
	}
	if got.UpdatedAt == nil || got.Spec.AllowedDomains == nil || !slices.Equal(*got.Spec.AllowedDomains, []string{"pypi.org"}) || len(got.Spec.WithheldGrants) != 1 {
		t.Errorf("GET = %+v, want the walls just saved with updated_at", got)
	}
	// Nobody else's: another person reads none.
	other := ssoSession(t, "sub-walls-other", "other@corp.example", oidc.RoleUser)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me/walls", other, ""); strings.TrimSpace(w.Body.String()) != `{"spec":{}}` {
		t.Errorf("another person read %s, want no walls", w.Body)
	}
	set := wallsAuditRow(h.audit.snapshot(), auditWallsSet)
	if set == nil || !strings.Contains(string(set.Data), `"withheld_kinds":["ssh_key"]`) || strings.Contains(string(set.Data), "pypi.org") {
		t.Errorf("%s row = %v, want the knobs and kinds, never the lists", auditWallsSet, set)
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/walls", member, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body)
	}
	if v := string(st.prefs[wallsSub+"|"+types.PersonalWallsPrefKey]); v != `{}` {
		t.Errorf("after DELETE the stored document is %q, want {}", v)
	}
	if wallsAuditRow(h.audit.snapshot(), auditWallsDelete) == nil {
		t.Errorf("no %s row", auditWallsDelete)
	}
	// The admin token is no person.
	if w := do(t, srv, http.MethodGet, "/api/v1/me/walls", adminToken, ""); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonWallsNeedPerson {
		t.Errorf("admin token GET = %d %s, want 400 %s", w.Code, w.Body, reasonWallsNeedPerson)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/me/walls", adminToken, `{"spec":{}}`); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonWallsNeedPerson {
		t.Errorf("admin token PUT = %d %s, want 400 %s", w.Code, w.Body, reasonWallsNeedPerson)
	}
	// A store that keeps no preferences.
	bare, _, _, _ := newAuthzMatrixServer(t)
	if w := doSSO(t, bare, http.MethodGet, "/api/v1/me/walls", member, ""); w.Code != http.StatusNotImplemented || wireReason(t, w) != reasonWallsUnavailable {
		t.Errorf("GET on a store without preferences = %d %s, want 501 %s", w.Code, w.Body, reasonWallsUnavailable)
	}
}

// TestPersonalWallsRunStep: the step meets the launching person's walls into the run's policy, drops
// the credentials they keep out, records each knob as theirs and names it; a caller who is no person,
// and a person with none, is untouched; walls that cannot be read refuse the run.
func TestPersonalWallsRunStep(t *testing.T) {
	srv, st, _ := uvServer(t)
	anthropic := types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.anthropic.com","header":"x-api-key","secret_name":"anthropic"}`)}
	saveWalls(t, st, wallsSub, types.PersonalWallsSpec{
		AllowedDomains: wallsList("pypi.org"), DeniedDomains: wallsList("evil.example"),
		Desktop: &types.DesktopControls{PasteIn: types.TransferBlocked}, WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey}},
	})
	open := func() types.RunPolicySpec {
		return types.RunPolicySpec{AllowAllEgress: true, EligibleGrants: []types.GrantSpec{anthropic, wallsSSH}}
	}

	spec := open()
	source := policySourceRecord{knobs: startProvenance(memberProvenance)}
	warns, refusal := srv.personalWallsRunStep(memberCtxFor(wallsSub), &spec, &source)
	if refusal != nil {
		t.Fatalf("refusal %+v", refusal)
	}
	if spec.AllowAllEgress || !slices.Equal(spec.AllowedDomains, []string{"pypi.org"}) || !slices.Contains(spec.DeniedDomains, "evil.example") {
		t.Errorf("egress = open %v allowed %v denied %v, want closed to pypi.org with evil.example denied", spec.AllowAllEgress, spec.AllowedDomains, spec.DeniedDomains)
	}
	if d := spec.Desktop.Effective(); d.PasteIn != types.TransferBlocked {
		t.Errorf("desktop = %+v, want paste_in blocked", d)
	}
	if len(spec.EligibleGrants) != 1 || spec.EligibleGrants[0].Kind != types.GrantAPIKey {
		t.Errorf("grants = %+v, want the ssh_key kept out and the api_key kept", spec.EligibleGrants)
	}
	for _, knob := range []string{"allow_all_egress", "allowed_domains", "denied_domains", "desktop", "eligible_grants"} {
		if source.knobs[knob] != personalProvenance {
			t.Errorf("provenance[%s] = %+v, want the person's walls", knob, source.knobs[knob])
		}
		if !slices.Contains(warns, "your walls narrowed "+knob) {
			t.Errorf("warnings %v do not name %s", warns, knob)
		}
	}
	if source.knobs["min_confinement_class"] != memberProvenance {
		t.Errorf("provenance[min_confinement_class] = %+v, want untouched", source.knobs["min_confinement_class"])
	}
	rec := source.PersonalWalls
	if rec == nil || len(rec.Removed["eligible_grants"]) != 1 || !slices.Equal(rec.Added["denied_domains"], []string{"evil.example"}) {
		t.Errorf("policy_source.personal_walls = %+v, want the ssh_key removed and evil.example added", rec)
	}

	// The admin token, and a person with no walls, are untouched.
	for name, ctx := range map[string]context.Context{"admin token": context.Background(), "no walls": memberCtxFor("sub-walls-none")} {
		spec, source := open(), policySourceRecord{knobs: startProvenance(memberProvenance)}
		if warns, refusal := srv.personalWallsRunStep(ctx, &spec, &source); refusal != nil || warns != nil || !spec.AllowAllEgress || source.PersonalWalls != nil {
			t.Errorf("%s: warns %v refusal %+v open %v record %+v, want nothing changed", name, warns, refusal, spec.AllowAllEgress, source.PersonalWalls)
		}
	}

	// Walls that cannot be read refuse the run.
	st.prefs["sub-walls-bad|"+types.PersonalWallsPrefKey] = json.RawMessage(`{"spec":{"bogus":true}}`)
	spec, source = open(), policySourceRecord{knobs: startProvenance(memberProvenance)}
	if _, refusal := srv.personalWallsRunStep(memberCtxFor("sub-walls-bad"), &spec, &source); refusal == nil || refusal.err == nil {
		t.Errorf("unreadable walls: refusal %+v, want a server error", refusal)
	}
}

// TestPersonalWallsOnEveryArm: a member's walls bind the default arm (the preview shows them) and the
// inline arm, where a New Run host outside the walls is recorded as clamped on run.create.
func TestPersonalWallsOnEveryArm(t *testing.T) {
	srv, st := wallsRunFixture(t)
	saveWalls(t, st, wallsSub, types.PersonalWallsSpec{AllowedDomains: wallsList("api.anthropic.com"), DeniedDomains: wallsList("evil.example")})
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodPost, previewDoor, member, `{"agent":"claude-code","task":"t"}`)
	var pv policyPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
		t.Fatalf("preview %d %v: %s", w.Code, err, w.Body)
	}
	if slices.Contains(pv.Spec.AllowedDomains, "pypi.org") || !slices.Contains(pv.Spec.DeniedDomains, "evil.example") {
		t.Errorf("default arm: allowed %v denied %v, want the walls met in", pv.Spec.AllowedDomains, pv.Spec.DeniedDomains)
	}

	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]},` +
		`"overrides":{"hosts":{"add_allowed":["pypi.org"]}}}`
	w = doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, body)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create = %d %v: %s", w.Code, err, w.Body)
	}
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), created.ID, "run.create", "success")
	var data struct {
		PolicySource struct {
			OverridesDiff *client.RunOverridesDiff `json:"overrides_diff"`
			PersonalWalls *wallsSourceRecord       `json:"personal_walls"`
		} `json:"policy_source"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if d := data.PolicySource.OverridesDiff; d == nil || !slices.Contains(d.Clamped, fieldAllowed+" pypi.org") {
		t.Errorf("inline arm: overrides_diff = %+v, want the added host outside the walls clamped", d)
	}
	if p := data.PolicySource.PersonalWalls; p == nil || !slices.Contains(p.Narrowed, "denied_domains") || !slices.Contains(p.Removed["allowed_domains"], "pypi.org") {
		t.Errorf("inline arm: personal_walls = %+v, want the denies added and pypi.org removed", p)
	}
}

// TestPersonalWallsSealLayer: the run owner's walls seal their denies, desktop blocks and withheld
// credentials, never their allowlist; a run with no owner or walls naming none of those has no layer;
// unreadable walls seal the run to no egress and no credential.
func TestPersonalWallsSealLayer(t *testing.T) {
	srv, st, _ := uvServer(t)
	saveWalls(t, st, wallsSub, types.PersonalWallsSpec{
		AllowedDomains: wallsList("pypi.org"), DeniedDomains: wallsList("evil.example"),
		Desktop: &types.DesktopControls{PasteIn: types.TransferBlocked}, WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey}},
	})
	saveWalls(t, st, "sub-walls-list", types.PersonalWallsSpec{AllowedDomains: wallsList("pypi.org")})
	st.prefs["sub-walls-bad|"+types.PersonalWallsPrefKey] = json.RawMessage(`not json`)
	ctx := context.Background()

	layers := srv.personalWallsSealLayer(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub})
	if len(layers) != 1 {
		t.Fatalf("layers = %+v, want one", layers)
	}
	l := layers[0]
	if l.rec != personalProvenance || l.egress.AllowedDomains != nil || l.egress.AllowAllEgress != nil || l.grants != nil {
		t.Errorf("layer = %+v, want the person's, with no allowlist, allow-all or grant list", l)
	}
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "github.com"}}
	sealPolicy(&spec, layers)
	if !slices.Equal(spec.AllowedDomains, []string{"api.anthropic.com", "github.com"}) || !slices.Contains(spec.DeniedDomains, "evil.example") {
		t.Errorf("sealed allowed %v denied %v, want the setup hosts kept and evil.example denied", spec.AllowedDomains, spec.DeniedDomains)
	}
	if d := spec.Desktop.Effective(); d.PasteIn != types.TransferBlocked || d.Upload != types.TransferAllowed {
		t.Errorf("sealed desktop %+v, want paste_in blocked and nothing else", d)
	}
	if w := (withholding{layers: layers}); !w.credentialWithheld(wallsSSH) || !w.withheldAny([]string{"evil.example"}) || w.withheldAny([]string{"github.com"}) {
		t.Error("the walls layer does not withhold the ssh_key and evil.example, or withholds github.com")
	}

	for name, run := range map[string]types.AgentRun{"no owner": {ID: uuid.New()}, "only an allowlist": {ID: uuid.New(), CreatedBy: "sub-walls-list"}} {
		if l := srv.personalWallsSealLayer(ctx, run); l != nil {
			t.Errorf("%s: layers %+v, want none", name, l)
		}
	}
	bad := srv.personalWallsSealLayer(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "sub-walls-bad"})
	if len(bad) != 1 || bad[0].egress.AllowedDomains == nil || len(*bad[0].egress.AllowedDomains) != 0 || bad[0].grants == nil || len(*bad[0].grants) != 0 {
		t.Errorf("unreadable walls: %+v, want the run sealed to no egress and no credential", bad)
	}
}

// TestPersonalWallsAdmitApproval: an approval of a host the run owner's walls deny is refused whoever
// decides, with the owner's own sentence for the owner; a host outside their allowlist is not.
func TestPersonalWallsAdmitApproval(t *testing.T) {
	srv, st, _ := uvServer(t)
	saveWalls(t, st, wallsSub, types.PersonalWallsSpec{AllowedDomains: wallsList("pypi.org"), DeniedDomains: wallsList("*.evil.example")})
	run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub}
	ask := func(host string) types.ApprovalRequest {
		return types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"` + host + `"}`)}
	}
	admit := func(decider, host string) (*httptest.ResponseRecorder, bool) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(memberCtxFor(decider))
		_, _, ok := srv.wallsAdmitApproval(w, r, ask(host), run, true)
		return w, ok
	}
	w, ok := admit(wallsSub, "a.evil.example")
	if ok || w.Code != http.StatusForbidden || wireReason(t, w) != reasonPersonalWallsHost || !strings.Contains(w.Body.String(), "Your walls block a.evil.example") {
		t.Errorf("owner approving a denied host: ok %v %d %s, want 403 %s in the owner's words", ok, w.Code, w.Body, reasonPersonalWallsHost)
	}
	var refusal errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &refusal)
	if refusal.Policy == nil || refusal.Policy.Layer != policyref.LayerPersonal || refusal.Policy.Owner != wallsSub {
		t.Errorf("the refusal's policy = %+v, want layer %s owned by %s", refusal.Policy, policyref.LayerPersonal, wallsSub)
	}
	w, ok = admit("sub-approver", "a.evil.example")
	if ok || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), wallsSub+"'s walls block a.evil.example") {
		t.Errorf("another approving a denied host: ok %v %d %s, want 403 naming the owner's walls", ok, w.Code, w.Body)
	}
	if _, ok := admit("sub-approver", "docs.example.com"); !ok {
		t.Error("a host outside the walls' allowlist was refused; it follows the walls' own unlisted-site choice")
	}
	st.prefs[wallsSub+"|"+types.PersonalWallsPrefKey] = json.RawMessage(`not json`)
	if w, ok := admit("sub-approver", "pypi.org"); ok || w.Code != http.StatusInternalServerError {
		t.Errorf("unreadable walls: ok %v %d, want 500", ok, w.Code)
	}
}

// TestReassertNamesOnlyTheLayersPresent: run.ceiling.reassert carries boundary only for a boundary
// layer and personal_walls for the walls' layer.
func TestReassertNamesOnlyTheLayersPresent(t *testing.T) {
	for _, tc := range []struct {
		name               string
		layer              sealLayer
		boundary, personal bool
	}{
		{"walls", sealLayer{rec: personalProvenance, egress: types.CeilingOverlay{DeniedDomains: wallsList("gitlab.example")}}, false, true},
		{"restriction", denyOnlyLayer("gitlab.example"), false, false},
		{"boundary", listLayer("boundary", []string{"api.anthropic.com"}), true, false},
	} {
		srv, _, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
		policy := types.RunPolicySpec{AllowAllEgress: true}
		injections := []runner.InjectionGrant{componentRule("api.anthropic.com"), componentRule("gitlab.example")}
		var p dispatchParams
		var llm llmTransport
		var bedrock []string
		srv.reassertCeilingDenies(context.Background(), run, &policy, &injections, dispatchCeiling{resolved: true}, []sealLayer{tc.layer},
			nil, &p, map[string]string{}, &llm, &bedrock, nil)
		ev := findAudit(audit.snapshot(), run.ID, "run.ceiling.reassert", "success")
		if ev == nil {
			t.Fatalf("%s: no run.ceiling.reassert row for a withheld lane", tc.name)
		}
		if got := strings.Contains(string(ev.Data), `"boundary":true`); got != tc.boundary {
			t.Errorf("%s: boundary %v, want %v (%s)", tc.name, got, tc.boundary, ev.Data)
		}
		if got := strings.Contains(string(ev.Data), `"personal_walls":true`); got != tc.personal {
			t.Errorf("%s: personal_walls %v, want %v (%s)", tc.name, got, tc.personal, ev.Data)
		}
	}
}
