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

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
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
	screen, fiveMin := types.RecordingScreenActions, 300
	on, off, empty, cc9, review := true, false, types.FirstUseMode("ask_me"), types.ConfinementClass("CC9"), types.FirstUseWaitForReview
	keystrokes, recOff, zero, negative := types.RecordingLevel("keystrokes"), types.RecordingOff, 0, -1
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
		{"an empty ado list", types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{}}, "azure_devops_capabilities:"},
		{"an unknown ado capability", types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeRead, "root"}}, "azure_devops_capabilities[1]"},
		{"an ado area no capability can be held over", types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapDiscovery}}, "azure_devops_capabilities[0]"},
		{"an empty github list", types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{}}, "github_capabilities:"},
		{"an unknown github capability", types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{"root"}}, "github_capabilities[0]"},
		{"a tool rule that allows", types.PersonalWallsSpec{ToolRules: &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}, {Tool: "Read", Effect: types.ToolAllow}}}, "tool_rules[1]"},
		{"an unknown tool effect", types.PersonalWallsSpec{ToolRules: &[]types.ToolRule{{Tool: "Bash", Effect: "ask"}}}, "tool_rules[0]"},
		{"a duplicate tool rule", types.PersonalWallsSpec{ToolRules: &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}, {Tool: "Bash", Effect: types.ToolDeny}}}, "tool_rules[1]"},
		{"an empty deny path", types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyPaths: wallsList("a/**", "")}}, "push_rules.deny_paths[1]"},
		{"too many deny paths", types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyPaths: &tooMany}}, "push_rules.deny_paths:"},
		{"deny_all false", types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyAll: &off}}, "push_rules.deny_all"},
		{"an unknown recording level", types.PersonalWallsSpec{Recording: &keystrokes}, "recording"},
		{"recording off", types.PersonalWallsSpec{Recording: &recOff}, "recording"},
		{"a zero auto-stop", types.PersonalWallsSpec{AutoStopAfterSec: &zero}, "auto_stop_after_sec"},
		{"a negative auto-stop", types.PersonalWallsSpec{AutoStopAfterSec: &negative}, "auto_stop_after_sec"},
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
		WithheldGrants:          []types.GrantSpec{{Kind: types.GrantSSHKey}},
		AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeRead}, GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead},
		ToolRules: &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}, {Tool: "*", Effect: types.ToolHold}},
		PushRules: &types.PersonalWallsPushRules{DenyPaths: wallsList("secrets/**", ".github/workflows/**"), DenyAll: &on},
		Recording: &screen, AutoStopAfterSec: &fiveMin,
	}
	if err := validatePersonalWalls(ok); err != nil {
		t.Errorf("valid walls refused: %v", err)
	}
	if knobs, _ := wallsAuditData(ok)["knobs"].([]string); !slices.Contains(knobs, "push_rules.deny_all") {
		t.Errorf("audit knobs = %v, want push_rules.deny_all", knobs)
	}
	if knobs, _ := wallsAuditData(types.PersonalWallsSpec{})["knobs"].([]string); slices.Contains(knobs, "push_rules.deny_all") {
		t.Errorf("walls naming nothing record push_rules.deny_all: %v", knobs)
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

// wallsRunStore is the walls fake with the runs a list of approvals reads back.
type wallsRunStore struct {
	*uvStore
	runs map[uuid.UUID]types.AgentRun
}

func (s *wallsRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if run, ok := s.runs[id]; ok {
		return run, nil
	}
	return types.AgentRun{}, store.ErrNotFound
}

// TestPersonalWallsProjectWallsBlock: only a PENDING egress_domain row whose host the run owner's walls
// deny is marked; a host outside the deny list, a decided row and a run that cannot be read are not.
func TestPersonalWallsProjectWallsBlock(t *testing.T) {
	h := newHarness(t)
	uv := &uvStore{permStore: &permStore{capStore: &capStore{userTypes: utKnown}}, prefs: map[string]json.RawMessage{}}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub}
	st := &wallsRunStore{uvStore: uv, runs: map[uuid.UUID]types.AgentRun{run.ID: run}}
	srv := New(baseTestConfig(h, st))
	saveWalls(t, uv, wallsSub, types.PersonalWallsSpec{DeniedDomains: wallsList("*.evil.example")})
	row := func(runID uuid.UUID, host string, state types.ApprovalState) types.ApprovalRequest {
		return types.ApprovalRequest{RunID: runID, Kind: types.ApprovalEgressDomain, State: state, RequestedScope: json.RawMessage(`{"host":"` + host + `"}`)}
	}
	rows := []types.ApprovalRequest{
		row(run.ID, "a.evil.example", types.ApprovalPending),
		row(run.ID, "docs.example.com", types.ApprovalPending),
		row(run.ID, "a.evil.example", types.ApprovalApproved),
		row(uuid.New(), "a.evil.example", types.ApprovalPending),
	}
	srv.projectWallsBlock(context.Background(), rows)
	for i, want := range []bool{true, false, false, false} {
		if rows[i].WallsBlock != want {
			t.Errorf("row %d (%s %s): walls_block = %v, want %v", i, rows[i].State, approvalHost(rows[i]), rows[i].WallsBlock, want)
		}
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
	var refusal errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &refusal)
	if ok || w.Code != http.StatusForbidden || wireReason(t, w) != reasonPersonalWallsHost || refusal.Error != "Your walls block a.evil.example. Change them in Settings." {
		t.Errorf("owner approving a denied host: ok %v %d %s, want 403 %s in the owner's words", ok, w.Code, w.Body, reasonPersonalWallsHost)
	}
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
		policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "gitlab.example"}} // a credential's host is listed exactly
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

// wallsMeetStep runs spec through personalWallsRunStep as sub, under an organisation whose recording block
// is org (nil for none), and answers the spec the walls left and the refusal, if any.
func wallsMeetStep(t *testing.T, srv *Server, st *uvStore, walls types.PersonalWallsSpec, org *types.SiteRecording, spec types.RunPolicySpec) (types.RunPolicySpec, *runRefusal) {
	t.Helper()
	const sub = "sub-walls-widen"
	saveWalls(t, st, sub, walls)
	st.site.Recording = org
	source := policySourceRecord{knobs: startProvenance(memberProvenance)}
	if _, refusal := srv.personalWallsRunStep(memberCtxFor(sub), &spec, &source); refusal != nil {
		return types.RunPolicySpec{}, refusal
	}
	return spec, nil
}

// TestPersonalWallsNeverWiden: the spec the run step is handed is already as narrow as the ceiling and the
// deployment make it. For each of the five narrowing knobs, walls wider than that spec leave it as it
// is, and walls narrower than it set their own value. Empty walls leave the spec byte for byte as it was.
func TestPersonalWallsNeverWiden(t *testing.T) {
	srv, st, _ := uvServer(t)
	run := func(walls types.PersonalWallsSpec, org *types.SiteRecording, spec types.RunPolicySpec) types.RunPolicySpec {
		t.Helper()
		got, refusal := wallsMeetStep(t, srv, st, walls, org, spec)
		if refusal != nil {
			t.Fatalf("refusal %+v", refusal)
		}
		return got
	}
	sec := func(n int) *int { return &n }
	rec := func(l types.RecordingLevel) *types.RecordingLevel { return &l }
	rules := func(r ...types.ToolRule) *[]types.ToolRule { return &r }
	read, write := adoscope.CapCodeRead, adoscope.CapCodeWrite

	t.Run("azure_devops_capabilities", func(t *testing.T) {
		ceiling := types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{read}}
		wider := types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{read, write}}
		if got := run(wider, nil, ceiling); !slices.Equal(got.AzureDevOpsCapabilities, []adoscope.Capability{read}) {
			t.Errorf("a wider list: %v, want the ceiling's [code_read]", got.AzureDevOpsCapabilities)
		}
		narrower := types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{read}}
		if got := run(narrower, nil, types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{read, write}}); !slices.Equal(got.AzureDevOpsCapabilities, []adoscope.Capability{read}) {
			t.Errorf("a narrower list: %v, want the wall's [code_read]", got.AzureDevOpsCapabilities)
		}
		// A policy that names no list falls back to the provider row's default profile, which create
		// does not know: the walls add no list here, and say nothing. Dispatch meets the default with
		// them (personal_walls_capabilities_test.go).
		if got := run(wider, nil, types.RunPolicySpec{}); len(got.AzureDevOpsCapabilities) != 0 {
			t.Errorf("no list in the policy: %v, want none added", got.AzureDevOpsCapabilities)
		}
		saveWalls(t, st, "sub-walls-widen", wider)
		noop := policySourceRecord{knobs: startProvenance(memberProvenance)}
		bare := types.RunPolicySpec{}
		warns, _ := srv.personalWallsRunStep(memberCtxFor("sub-walls-widen"), &bare, &noop)
		if slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "azure_devops_capabilities") }) {
			t.Errorf("no list in the policy: warnings %v, want none naming the list: dispatch applies it", warns)
		}
		// A list with nothing in common is refused with the person's own words, never a server error.
		_, refusal := wallsMeetStep(t, srv, st, types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{write}}, nil, ceiling)
		if refusal == nil || refusal.err != nil || refusal.status != http.StatusForbidden || refusal.body.Reason != reasonGovernanceOverlayUnsatisfiable ||
			!strings.Contains(refusal.body.Error, "azure_devops_capabilities") {
			t.Errorf("a disjoint list: %+v, want 403 %s naming azure_devops_capabilities", refusal, reasonGovernanceOverlayUnsatisfiable)
		}
	})

	t.Run("github_capabilities", func(t *testing.T) {
		ceiling := types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead}}
		wider := types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}}
		if got := run(wider, nil, ceiling); !slices.Equal(got.GitHubCapabilities, []ghscope.Capability{ghscope.CapCodeRead}) {
			t.Errorf("a wider list: %v, want the ceiling's [code_read]", got.GitHubCapabilities)
		}
		narrower := types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead}}
		if got := run(narrower, nil, types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}}); !slices.Equal(got.GitHubCapabilities, []ghscope.Capability{ghscope.CapCodeRead}) {
			t.Errorf("a narrower list: %v, want the wall's [code_read]", got.GitHubCapabilities)
		}
		saveWalls(t, st, "sub-walls-widen", wider)
		noop := policySourceRecord{knobs: startProvenance(memberProvenance)}
		bare := types.RunPolicySpec{}
		warns, _ := srv.personalWallsRunStep(memberCtxFor("sub-walls-widen"), &bare, &noop)
		if slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "github_capabilities") }) || len(bare.GitHubCapabilities) != 0 {
			t.Errorf("no list in the policy: warnings %v list %v, want neither: the GitHub CLI's grants apply it", warns, bare.GitHubCapabilities)
		}
	})

	t.Run("tool_rules", func(t *testing.T) {
		ceiling := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}, {Tool: "Bash", Effect: types.ToolDeny}, {Tool: "Read", Effect: types.ToolAllow}}}
		loose := types.PersonalWallsSpec{ToolRules: rules(types.ToolRule{Tool: "Bash", Effect: types.ToolHold})}
		if got := run(loose, nil, ceiling); !slices.Equal(got.ToolRules, ceiling.ToolRules) {
			t.Errorf("a hold under the ceiling's deny: %v, want the ceiling's %v", got.ToolRules, ceiling.ToolRules)
		}
		// Adding a rule leaves every tool the wall does not name as the policy had it.
		got := run(types.PersonalWallsSpec{ToolRules: rules(types.ToolRule{Tool: "WebFetch", Effect: types.ToolDeny})}, nil, ceiling)
		want := append(slices.Clone(ceiling.ToolRules), types.ToolRule{Tool: "WebFetch", Effect: types.ToolDeny})
		if !slices.Equal(got.ToolRules, want) {
			t.Errorf("a deny on one tool: %v, want the ceiling's rules plus WebFetch deny", got.ToolRules)
		}
		got = run(types.PersonalWallsSpec{ToolRules: rules(types.ToolRule{Tool: "Read", Effect: types.ToolDeny})}, nil, ceiling)
		if eff := effectiveToolEffect(got.ToolRules, "Read"); eff != types.ToolDeny || effectiveToolEffect(got.ToolRules, "Glob") != types.ToolAllow {
			t.Errorf("a deny on Read: %v, want Read denied and an unnamed tool still allowed", got.ToolRules)
		}
		// The order of a document's rules does not matter: a rule for a named tool beats "*" in either, and
		// an inherited deny is kept.
		star, bashHold := types.ToolRule{Tool: "*", Effect: types.ToolDeny}, types.ToolRule{Tool: "Bash", Effect: types.ToolHold}
		for _, doc := range [][]types.ToolRule{{star, bashHold}, {bashHold, star}} {
			wall := types.PersonalWallsSpec{ToolRules: rules(doc...)}
			got = run(wall, nil, types.RunPolicySpec{})
			if effectiveToolEffect(got.ToolRules, "Bash") != types.ToolHold || effectiveToolEffect(got.ToolRules, "Glob") != types.ToolDeny {
				t.Errorf("walls %v over no rules: %+v, want Bash held and an unnamed tool denied", doc, got.ToolRules)
			}
			got = run(wall, nil, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}})
			if effectiveToolEffect(got.ToolRules, "Bash") != types.ToolDeny || effectiveToolEffect(got.ToolRules, "Glob") != types.ToolDeny {
				t.Errorf("walls %v over an inherited Bash deny: %+v, want Bash still denied", doc, got.ToolRules)
			}
		}
		// A policy with no rules gets none it did not ask for beyond the wall's own.
		got = run(types.PersonalWallsSpec{ToolRules: rules(types.ToolRule{Tool: "Bash", Effect: types.ToolHold})}, nil, types.RunPolicySpec{})
		if !slices.Equal(got.ToolRules, []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}) {
			t.Errorf("a hold on a policy with no rules: %v, want just the hold", got.ToolRules)
		}
	})

	t.Run("push_rules.deny_paths", func(t *testing.T) {
		ceiling := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{"secrets/**"}}}
		wall := types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyPaths: wallsList("secrets/**")}}
		if got := run(wall, nil, ceiling); got.PushRules == nil || !slices.Equal(got.PushRules.DenyPaths, []string{"secrets/**"}) {
			t.Errorf("a path the ceiling already denies: %+v, want the ceiling's list as it was", got.PushRules)
		}
		// Paths add to the ceiling's, never take one away.
		wall = types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyPaths: wallsList(".github/workflows/**")}}
		got := run(wall, nil, ceiling)
		if got.PushRules == nil || !slices.Contains(got.PushRules.DenyPaths, "secrets/**") || !slices.Contains(got.PushRules.DenyPaths, ".github/workflows/**") {
			t.Errorf("an added path: %+v, want the ceiling's path and the wall's", got.PushRules)
		}
		if got := run(wall, nil, types.RunPolicySpec{}); got.PushRules == nil || !slices.Equal(got.PushRules.DenyPaths, []string{".github/workflows/**"}) {
			t.Errorf("a path on a policy with no push rules: %+v, want just the wall's", got.PushRules)
		}
	})

	t.Run("push_rules.deny_all", func(t *testing.T) {
		on := true
		wall := types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyAll: &on}}
		ceiling := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{"secrets/**"}}}
		if got := run(wall, nil, ceiling); got.PushRules == nil || !got.PushRules.DenyAll || !slices.Equal(got.PushRules.DenyPaths, []string{"secrets/**"}) {
			t.Errorf("deny_all over a policy with paths: %+v, want deny_all and the policy's paths", got.PushRules)
		}
		if got := run(wall, nil, types.RunPolicySpec{}); got.PushRules == nil || !got.PushRules.DenyAll {
			t.Errorf("deny_all on a policy with no push rules: %+v, want deny_all", got.PushRules)
		}
	})

	t.Run("recording", func(t *testing.T) {
		if got := run(types.PersonalWallsSpec{Recording: rec(types.RecordingTerminal)}, nil, types.RunPolicySpec{Recording: types.RecordingScreenActions}); got.Recording != types.RecordingScreenActions {
			t.Errorf("a lower floor under the ceiling's: %q, want screen_actions", got.Recording)
		}
		if got := run(types.PersonalWallsSpec{Recording: rec(types.RecordingScreenActions)}, nil, types.RunPolicySpec{Recording: types.RecordingTerminal}); got.Recording != types.RecordingScreenActions {
			t.Errorf("a higher floor: %q, want the wall's screen_actions", got.Recording)
		}
		// A policy that names no level stays naming none at create: the floor is applied at dispatch, after
		// the organisation's default, so it can only raise the level the run would have had.
		resolved := func(walls types.PersonalWallsSpec, org *types.SiteRecording, spec types.RunPolicySpec) types.RecordingLevel {
			t.Helper()
			got := run(walls, org, spec)
			owner := types.AgentRun{CreatedBy: "sub-walls-widen"}
			floor, err := srv.wallsRecordingFloor(context.Background(), owner)
			if err != nil {
				t.Fatalf("read the floor: %v", err)
			}
			return recordingChoice(owner, got, dispatchCeiling{}, types.SiteConfig{Recording: org}, nil, floor).Resolve()
		}
		unlocked := &types.SiteRecording{Level: types.RecordingScreenActions}
		if got := run(types.PersonalWallsSpec{Recording: rec(types.RecordingTerminal)}, unlocked, types.RunPolicySpec{}); got.Recording != "" {
			t.Errorf("a floor on a policy that names no level: %q, want none pinned at create", got.Recording)
		}
		if got := resolved(types.PersonalWallsSpec{Recording: rec(types.RecordingTerminal)}, unlocked, types.RunPolicySpec{}); got != types.RecordingScreenActions {
			t.Errorf("a floor under an unlocked organisation default: %q, want screen_actions", got)
		}
		terminalOrg := &types.SiteRecording{Level: types.RecordingTerminal}
		if got := resolved(types.PersonalWallsSpec{Recording: rec(types.RecordingScreenActions)}, terminalOrg, types.RunPolicySpec{}); got != types.RecordingScreenActions {
			t.Errorf("a floor over the organisation's terminal: %q, want screen_actions", got)
		}
		if got := resolved(types.PersonalWallsSpec{Recording: rec(types.RecordingScreenActions)}, nil, types.RunPolicySpec{}); got != types.RecordingScreenActions {
			t.Errorf("a floor over the terminal default: %q, want screen_actions", got)
		}
		// An organisation default raised after create still counts: nothing was pinned from the old one.
		if got := resolved(types.PersonalWallsSpec{Recording: rec(types.RecordingTerminal)}, &types.SiteRecording{Level: types.RecordingScreenActions, Locked: true}, types.RunPolicySpec{}); got != types.RecordingScreenActions {
			t.Errorf("a floor under a locked organisation level: %q, want screen_actions", got)
		}
		// Walls that cannot be read at dispatch refuse the run: no level, never a lower one.
		st.prefs["sub-walls-bad|"+types.PersonalWallsPrefKey] = json.RawMessage(`{"spec":{"bogus":true}}`)
		if floor, err := srv.wallsRecordingFloor(context.Background(), types.AgentRun{CreatedBy: "sub-walls-bad"}); err == nil || floor != "" {
			t.Errorf("unreadable walls: floor %q err %v, want an error and no level", floor, err)
		}
	})

	t.Run("auto_stop_after_sec", func(t *testing.T) {
		if got := run(types.PersonalWallsSpec{AutoStopAfterSec: sec(900)}, nil, types.RunPolicySpec{AutoStopAfterSec: 300}); got.AutoStopAfterSec != 300 {
			t.Errorf("a longer cap under the ceiling's: %d, want 300", got.AutoStopAfterSec)
		}
		if got := run(types.PersonalWallsSpec{AutoStopAfterSec: sec(60)}, nil, types.RunPolicySpec{AutoStopAfterSec: 300}); got.AutoStopAfterSec != 60 {
			t.Errorf("a shorter cap: %d, want the wall's 60", got.AutoStopAfterSec)
		}
		for _, never := range []int{0, -1} {
			if got := run(types.PersonalWallsSpec{AutoStopAfterSec: sec(600)}, nil, types.RunPolicySpec{AutoStopAfterSec: never}); got.AutoStopAfterSec != 600 {
				t.Errorf("a cap on a policy that never stops (%d): %d, want the wall's 600", never, got.AutoStopAfterSec)
			}
		}
	})

	t.Run("empty walls change nothing", func(t *testing.T) {
		spec := types.RunPolicySpec{
			AllowAllEgress: true, AzureDevOpsCapabilities: []adoscope.Capability{read, write}, GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead},
			ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}, PushRules: &types.PushRulesSpec{DenyPaths: []string{"a"}},
			Recording: types.RecordingTerminal, AutoStopAfterSec: 300,
		}
		before := mustJSON(spec)
		for name, walls := range map[string]types.PersonalWallsSpec{
			"none saved": {}, "naming no rule": {ToolRules: rules()}, "naming no path": {PushRules: &types.PersonalWallsPushRules{}},
		} {
			if got := run(walls, nil, spec); !slices.Equal(mustJSON(got), before) {
				t.Errorf("%s: %s, want %s", name, mustJSON(got), before)
			}
		}
	})
}
