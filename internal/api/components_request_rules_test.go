// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// None of these tests calls t.Parallel: the on-switch ones swap the site feature hooks (withSiteFeatureHooks).

// rulesComponent is an inline component on host whose one request rule allows GET on it.
func rulesComponent(host string, secrets ...map[string]any) map[string]any {
	ref := inlineComponent([]string{host}, secrets...)
	ref["inline"].(map[string]any)["request_rules"] = []any{map[string]any{"host": host, "methods": []string{"GET"}, "effect": "allow"}}
	return ref
}

// rulesDefinition is rulesComponent's definition as the store holds it.
func rulesDefinition(host string) types.ComponentDefinition {
	return types.ComponentDefinition{Hosts: []string{host},
		RequestRules: []types.HostRule{{Host: host, Methods: []string{"GET"}, Effect: types.HostRuleAllow}}}
}

// requestRulesOn turns the switch on for one test: this release can turn the key on (the hook) and the site config
// stores it enabled. siteFeatureOn needs both.
func requestRulesOn(t *testing.T, sc *types.SiteConfig) {
	t.Helper()
	withSiteFeatureHooks(t, availableSiteFeatures(featureComponentRequestRules))
	on := true
	sc.FeatureSwitches = &types.SiteFeatureSwitches{ComponentRequestRules: &types.ComponentRequestRulesFeature{Enabled: &on}}
}

func policyRuleSet(host string) types.HostRuleSet {
	return types.HostRuleSet{Rules: []types.HostRule{{Host: host, Effect: types.HostRuleAllow}}}
}

// rulesSiteConfigFails is the matrix store with the one site config read the save doors' switch makes failing, and
// only that read: every other reader of a request still gets the stored config.
type rulesSiteConfigFails struct{ *authzStore }

func (s rulesSiteConfigFails) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	if pc, _, _, ok := runtime.Caller(1); ok && strings.HasSuffix(runtime.FuncForPC(pc).Name(), ".componentRequestRulesRefusal") {
		return types.SiteConfig{}, errors.New("conn closed by peer")
	}
	return s.authzStore.GetSiteConfig(ctx)
}

// requestRulesSaveServer is componentsServer with the run fixture's two secret values stored (the operator's, and
// the member's own), optionally behind a site config read that fails at the switch.
func requestRulesSaveServer(t *testing.T, failSiteConfig bool) (*Server, *authzStore, *recRecorder) {
	t.Helper()
	ast := newAuthzStore()
	h := newHarness(t)
	var st store.Store = ast
	if failSiteConfig {
		st = rulesSiteConfigFails{ast}
	}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{compOperatorSecret: []byte("operator-value")},
		owned: map[string]map[string][]byte{"sub-member": {compOwnSecret: []byte("own-value")}}}
	cfg.SessionRevocations = fakeAuthzSessionRevocations{}
	return matrixServer(cfg), ast, h.audit
}

const rulesHost = "rules.example"

// The wire spellings of a definition on rulesHost: with the stored rule, with it sent back with empty lists, with a
// changed rule, and with none.
var (
	rulesStoredDef  = `{"hosts":["rules.example"],"request_rules":[{"host":"rules.example","effect":"allow"}]}`
	rulesEmptyLists = `{"hosts":["rules.example"],"request_rules":[{"host":"rules.example","methods":[],"paths":[],"effect":"allow"}]}`
	rulesChangedDef = `{"hosts":["rules.example"],"request_rules":[{"host":"rules.example","effect":"deny"}]}`
	rulesNoneDef    = `{"hosts":["rules.example"]}`
)

func rulesStoredDefinition() types.ComponentDefinition {
	return types.ComponentDefinition{Hosts: []string{rulesHost}, RequestRules: []types.HostRule{{Host: rulesHost, Effect: types.HostRuleAllow}}}
}

// saveRoute is one save door, asked with a definition. stored seeds the row a PUT replaces (nil: a new row, or for
// the POST no row at all) and returns its path.
type saveRoute struct {
	name    string
	method  string
	created int // the status of an accepted save
	path    func(ast *authzStore, stored *types.ComponentDefinition) string
	cookie  func(t *testing.T) *http.Cookie
}

func saveRoutes() []saveRoute {
	member := func(t *testing.T) *http.Cookie { return ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser) }
	admin := func(t *testing.T) *http.Cookie { return ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin) }
	seed := func(ast *authzStore, owner string, def types.ComponentDefinition) uuid.UUID {
		c, err := ast.CreateComponent(context.Background(), types.Component{ID: uuid.New(), Owner: owner, Name: "Rules", Definition: def})
		if err != nil {
			panic(err)
		}
		return c.ID
	}
	return []saveRoute{
		{name: "POST /me/components", method: http.MethodPost, created: http.StatusCreated, cookie: member,
			path: func(*authzStore, *types.ComponentDefinition) string { return "/api/v1/me/components" }},
		{name: "PUT /me/components/{id}", method: http.MethodPut, created: http.StatusOK, cookie: member,
			path: func(ast *authzStore, stored *types.ComponentDefinition) string {
				def := types.ComponentDefinition{Hosts: []string{rulesHost}}
				if stored != nil {
					def = *stored
				}
				return "/api/v1/me/components/" + seed(ast, "sub-member", def).String()
			}},
		{name: "PUT /components/{id}, an update", method: http.MethodPut, created: http.StatusOK, cookie: admin,
			path: func(ast *authzStore, stored *types.ComponentDefinition) string {
				def := types.ComponentDefinition{Hosts: []string{rulesHost}}
				if stored != nil {
					def = *stored
				}
				return "/api/v1/components/" + seed(ast, "", def).String()
			}},
		{name: "PUT /components/{id}, a new row", method: http.MethodPut, created: http.StatusCreated, cookie: admin,
			path: func(*authzStore, *types.ComponentDefinition) string { return "/api/v1/components/" + uuid.NewString() }},
	}
}

// TestRequestRulesSaveDoors: the switch at the save routes. At each save route, while the switch is off, new or changed rules are
// refused with component_request_rules_off, and only the stored rules sent back (also spelled with empty methods and
// paths lists), or no rules, are accepted. An unreadable site config refuses a change and lets an unchanged save
// through. With the switch on, rules save.
func TestRequestRulesSaveDoors(t *testing.T) {
	stored := rulesStoredDefinition()
	for _, route := range saveRoutes() {
		updates := route.name != "POST /me/components" && route.name != "PUT /components/{id}, a new row"
		type step struct {
			name        string
			stored      *types.ComponentDefinition
			def         string
			fail, on    bool
			wantReason  string // "" = accepted
			wantMessage string
		}
		steps := []step{
			{name: "new rules", def: rulesStoredDef, wantReason: reasonComponentRequestRulesOff, wantMessage: componentRequestRulesOffMsg},
			{name: "no rules", def: rulesNoneDef},
			{name: "new rules, site config unreadable", def: rulesStoredDef, fail: true, wantReason: reasonComponentRequestRulesOff, wantMessage: componentRequestRulesUnreadMsg},
			{name: "new rules, switch on", def: rulesStoredDef, on: true},
		}
		if updates {
			steps = append(steps,
				step{name: "changed rules", stored: &stored, def: rulesChangedDef, wantReason: reasonComponentRequestRulesOff, wantMessage: componentRequestRulesOffMsg},
				step{name: "the stored rules sent back", stored: &stored, def: rulesStoredDef},
				step{name: "the stored rules sent back with empty lists", stored: &stored, def: rulesEmptyLists},
				step{name: "the stored rules removed", stored: &stored, def: rulesNoneDef},
				step{name: "the stored rules sent back, site config unreadable", stored: &stored, def: rulesStoredDef, fail: true},
				step{name: "changed rules, site config unreadable", stored: &stored, def: rulesChangedDef, fail: true,
					wantReason: reasonComponentRequestRulesOff, wantMessage: componentRequestRulesUnreadMsg},
				step{name: "changed rules, switch on", stored: &stored, def: rulesChangedDef, on: true},
			)
		}
		for _, s := range steps {
			t.Run(route.name+"/"+s.name, func(t *testing.T) {
				srv, ast, _ := requestRulesSaveServer(t, s.fail)
				if s.on {
					requestRulesOn(t, &ast.siteCfg)
				}
				path := route.path(ast, s.stored)
				w := doSSO(t, srv, route.method, path, route.cookie(t), saveComponentBody("Rules", s.def))
				if s.wantReason == "" {
					if w.Code != route.created {
						t.Fatalf("= %d %s, want %d", w.Code, w.Body.String(), route.created)
					}
					return
				}
				got := decodeErrorBody(t, w)
				if w.Code != http.StatusUnprocessableEntity || got.Reason != s.wantReason || got.Error != s.wantMessage {
					t.Fatalf("= %d %q %q, want 422 %q %q", w.Code, got.Reason, got.Error, s.wantReason, s.wantMessage)
				}
				if updates {
					// The refused save wrote nothing: the row keeps what it had.
					id := uuid.MustParse(path[strings.LastIndex(path, "/")+1:])
					owner := ""
					if strings.HasPrefix(path, "/api/v1/me/") {
						owner = "sub-member"
					}
					row, err := ast.GetComponent(context.Background(), id, owner)
					if err != nil || row.Version != 1 {
						t.Errorf("row after a refused save = %+v, %v; want it unchanged at version 1", row, err)
					}
				}
			})
		}
	}
}

// TestRequestRulesCreateDoors: the switch at the run doors, for every source a definition comes from. While the switch is off, a component that carries rules is
// refused at every run door, whichever source it comes from (inline, the person's saved row, the organisation's row,
// and so a template's run, which arrives as a create request), never attached without them. A component tool
// (tools[]) is never attached at this release at all. With the switch on, each source is admitted.
func TestRequestRulesCreateDoors(t *testing.T) {
	const (
		offInline = "components[0]: inline.request_rules: request rules are off on this deployment; an admin turns them on with features.component_request_rules.enabled"
		offSaved  = "components[0]: definition.request_rules: request rules are off on this deployment; an admin turns them on with features.component_request_rules.enabled"
		offOrg    = "components[0]: this component carries request rules, which are off on this deployment. Ask your admin."
	)
	ownID := uuid.New()
	sources := []struct {
		name     string
		ref      func(f *componentFixture) any
		sentence string
	}{
		{"inline", func(*componentFixture) any { return rulesComponent(rulesHost) }, offInline},
		{"the person's saved row", func(f *componentFixture) any {
			f.st.components = append(f.st.components, types.Component{ID: ownID, Owner: capSub, Name: "My Rules", Version: 1, Definition: rulesDefinition(rulesHost)})
			return map[string]any{"id": ownID.String()}
		}, offSaved},
		{"the organisation's row", func(f *componentFixture) any { return f.org(compOrgID, rulesDefinition(rulesHost)) }, offOrg},
	}
	for _, src := range sources {
		t.Run(src.name+"/off", componentCase{name: src.name, status: http.StatusUnprocessableEntity, reason: reasonComponentRequestRulesOff,
			sentence: src.sentence, setup: func(f *componentFixture) []any { return []any{src.ref(f)} }}.run)
		t.Run(src.name+"/off, nothing left behind", func(t *testing.T) {
			f := newComponentFixture(t)
			if w := f.ask(t, componentDoors[0], componentBody(src.ref(f))); w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("create = %d %s, want 422", w.Code, w.Body.String())
			}
			if len(f.st.runs) != 0 || len(f.st.snapshots) != 0 {
				t.Errorf("a refused create left %d runs and %d snapshots", len(f.st.runs), len(f.st.snapshots))
			}
			want := []string{` {"ordinal":0,"reason":"component_request_rules_off","source":"inline"}`}
			if src.name != "inline" {
				if rows := f.componentAudit(0, "run.component.refuse"); len(rows) != 1 || !strings.Contains(rows[0], `"reason":"component_request_rules_off"`) {
					t.Errorf("run.component.refuse = %v, want one row with the reason", rows)
				}
				return
			}
			if rows := f.componentAudit(0, "run.component.refuse"); !slices.Equal(rows, want) {
				t.Errorf("run.component.refuse = %v, want %v", rows, want)
			}
		})
		t.Run(src.name+"/on", func(t *testing.T) {
			f := newComponentFixture(t)
			requestRulesOn(t, &f.st.siteConfig)
			for _, door := range componentDoors {
				w := f.ask(t, door, componentBody(src.ref(f)))
				if w.Code >= 300 {
					t.Fatalf("%s = %d %s, want success", door, w.Code, w.Body.String())
				}
				f.st.components = nil // the next door's ref stores its row again
				if door == policyPreviewPath {
					// The preview's spec is the compiled policy: the component's rules are one set of it.
					var preview policyPreviewResponse
					if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
						t.Fatal(err)
					}
					set := types.HostRuleSet{Rules: rulesDefinition(rulesHost).RequestRules}
					if !slices.ContainsFunc(preview.Spec.HostRules, func(s types.HostRuleSet) bool { return reflect.DeepEqual(s, set) }) {
						t.Errorf("preview host_rules = %+v, want the component's set %+v", preview.Spec.HostRules, set)
					}
				}
				if door != componentDoors[0] {
					continue
				}
				// Launch attaches the component with its rules: the run's snapshot holds them.
				var created struct {
					ID uuid.UUID `json:"id"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
					t.Fatal(err)
				}
				f.st.cmu.Lock()
				snap := f.st.snapshots[created.ID]
				f.st.cmu.Unlock()
				if len(snap) != 1 || !reflect.DeepEqual(snap[0].Definition.RequestRules, rulesDefinition(rulesHost).RequestRules) {
					t.Errorf("launch snapshot = %+v, want the one component with its rules", snap)
				}
			}
		})
	}
	// A component tool carries its definition through the shape check, rules included, and is then refused as a
	// carrier this release does not honour: it is never attached, with or without its rules.
	tool := func(rules string) string {
		return modeBody(`"experience":"interactive","tools":[{"id":"lint","kind":"component","component":{"inline":{"hosts":["rules.example"],"request_rules":[` +
			rules + `]}}}],` + noRepos)
	}
	for _, tc := range []struct {
		name, rules, reason string
		status              int
	}{
		{"well-formed rules", `{"host":"rules.example","effect":"allow"}`, reasonRequestFieldUnavailable, http.StatusUnprocessableEntity},
		{"a rule on a host it does not name", `{"host":"other.example","effect":"allow"}`, reasonRunModeInvalid, http.StatusBadRequest},
	} {
		for _, door := range contractDoors {
			t.Run("tools[]/"+tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
				h.srv.cfg.AgentImages = composeAgentImages
				w := do(t, h.srv, http.MethodPost, door.path, adminToken, tool(tc.rules))
				if w.Code != tc.status || errorReason(w) != tc.reason {
					t.Fatalf("= %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.reason)
				}
			})
		}
	}
}

// rulesRequest is a create request of n inline components, each with its own host and one rule on it.
func rulesRequest(n int) createRunRequest {
	var req createRunRequest
	for i := 0; i < n; i++ {
		def := rulesDefinition(fmt.Sprintf("r%d.example", i))
		req.Components = append(req.Components, types.ComponentRef{Inline: &def})
	}
	return req
}

// rulesLimitSetup is the parity fixture one set past the limit: the switch on, the policy's own set, and
// MaxComponentRefs components with one set each.
func rulesLimitSetup(t *testing.T, f *componentFixture) map[string]any {
	requestRulesOn(t, &f.st.siteConfig)
	f.srv.cfg.DefaultPolicy.HostRules = []types.HostRuleSet{policyRuleSet("policy-rules.example")}
	refs := make([]any, types.MaxComponentRefs)
	for i := range refs {
		refs[i] = rulesComponent(fmt.Sprintf("r%d.example", i))
	}
	return componentBody(refs...)
}

// TestRequestRulesSetLimit: the host rule set limit at run create. A run whose host rule sets, its policy's (the ceiling's among them) with one per
// component that carries rules, would number more than 16 is refused before anything is expanded, audited at launch,
// and never trimmed; at 16 it is admitted.
func TestRequestRulesSetLimit(t *testing.T) {
	f := newComponentFixture(t)
	requestRulesOn(t, &f.st.siteConfig)
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))

	// The policy's one set and 15 components' sets: 16, admitted.
	spec := govDeployment()
	spec.HostRules = []types.HostRuleSet{policyRuleSet("policy-rules.example")}
	if _, refusal := f.srv.applyRunComponents(r, rulesRequest(types.MaxHostRuleSets-1), &spec, governanceCeiling{Spec: govDeployment()}, nil, true); refusal != nil {
		t.Fatalf("16 sets: refusal = %+v, want admitted", refusal.body)
	}
	if len(spec.HostRules) != types.MaxHostRuleSets {
		t.Errorf("16 sets: the spec carries %d sets, want %d", len(spec.HostRules), types.MaxHostRuleSets)
	}

	// One more: 17, refused, the spec untouched.
	spec = govDeployment()
	spec.HostRules = []types.HostRuleSet{policyRuleSet("policy-rules.example")}
	before, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	_, refusal := f.srv.applyRunComponents(r, rulesRequest(types.MaxHostRuleSets), &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
	if refusal == nil || refusal.body.Reason != reasonComponentRequestRulesLimit || refusal.status != http.StatusUnprocessableEntity {
		t.Fatalf("17 sets: refusal = %+v, want 422 %s", refusal, reasonComponentRequestRulesLimit)
	}
	if want := "This run would carry more than 16 host rule sets. Attach fewer components with request rules."; refusal.body.Error != want {
		t.Errorf("17 sets: sentence = %q, want %q", refusal.body.Error, want)
	}
	if after, _ := json.Marshal(spec); string(after) != string(before) {
		t.Errorf("17 sets: the refusal changed the spec: %s, was %s", after, before)
	}

	// The same count through the doors, the policy's set coming from the governance ceiling: each door answers the
	// same, launch audits it, and nothing is created.
	f = newComponentFixture(t)
	requestRulesOn(t, &f.st.siteConfig)
	ceiling := govDeployment()
	ceiling.HostRules = []types.HostRuleSet{policyRuleSet("ceiling-rules.example")}
	f.cs.govProfile = &types.GovernanceUnit{ID: uuid.New(), Name: "rules-ceiling", Ceiling: ceiling}
	f.cs.govTier = types.CapabilitySubjectUser
	refs := make([]any, types.MaxComponentRefs)
	for i := range refs {
		refs[i] = rulesComponent(fmt.Sprintf("r%d.example", i))
	}
	var first string
	for i, door := range componentDoors {
		before := len(f.rec.snapshot())
		w := f.ask(t, door, componentBody(refs...))
		if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != reasonComponentRequestRulesLimit {
			t.Fatalf("%s = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonComponentRequestRulesLimit)
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("%s answered %s, launch answered %s", door, w.Body.String(), first)
		}
		rows := f.componentAudit(before, "run.component.refuse")
		if want := []string{` {"reason":"component_request_rules_limit"}`}; i == 0 && !slices.Equal(rows, want) {
			t.Errorf("launch run.component.refuse = %v, want %v", rows, want)
		}
	}
	if len(f.st.runs) != 0 || len(f.st.snapshots) != 0 {
		t.Errorf("a refused request left %d runs and %d snapshots", len(f.st.runs), len(f.st.snapshots))
	}
}

// fillEvery sets every settable field reachable from v to a non-zero value: a pointer to a filled value, a slice or
// map to one filled entry, a string to a host name. depth bounds a type that reaches itself.
func fillEvery(v reflect.Value, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillEvery(v.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				fillEvery(f, depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillEvery(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillEvery(k, depth+1)
		fillEvery(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString("seal.example")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}

// TestRequestRulesSetLimitCountsSealLayers: no seal layer adds a host rule set, so a sealed run never meets past the limit. The set count is made at create because the seal adds no host rule
// set. Each seal layer is built the way the seal builds it, from a document with every field set, and carries no host
// rules; and the seal over a run that already holds 16 sets leaves its sets and its allowlist as they were. A layer
// that gains host rules reddens this test, and the count must then cover the seal.
func TestRequestRulesSetLimitCountsSealLayers(t *testing.T) {
	var boundary types.BoundarySpec
	fillEvery(reflect.ValueOf(&boundary).Elem(), 0)
	if ov := boundarySealEgress(boundary); ov.HostRules != nil {
		t.Errorf("the boundary's seal layer carries host rules: %+v", *ov.HostRules)
	}
	restriction := types.Restriction{ID: uuid.New(), Name: "every field"}
	fillEvery(reflect.ValueOf(&restriction.Spec).Elem(), 0)
	var walls types.PersonalWallsSpec
	fillEvery(reflect.ValueOf(&walls).Elem(), 0)
	run := types.AgentRun{ID: uuid.New(), WallPreset: &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network",
		Scope: types.WallPresetScopeBuiltin, Version: 1, Spec: walls}}
	if !noNetworkPreset(run.WallPreset) {
		t.Fatal("the run's preset is not the No network preset")
	}
	layers := append([]sealLayer{restrictionSealLayer(restriction)}, wallsSealLayer(personalProvenance, walls)...)
	layers = append(layers, wallPresetSealLayer(run)...)
	if len(layers) < 3 {
		t.Fatalf("seal layers = %d, want the restriction's, the walls' and the preset's", len(layers))
	}
	for _, l := range layers {
		if l.egress.HostRules != nil {
			t.Errorf("the %s seal layer carries host rules: %+v", l.rec.Layer, *l.egress.HostRules)
		}
	}

	// The seal over a run at the limit: layers that bound egress without emptying it change neither its sets nor its
	// allowlist, as they would if a meet had passed 16 sets and failed closed.
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.example"}}
	for i := 0; i < types.MaxHostRuleSets; i++ {
		spec.HostRules = append(spec.HostRules, policyRuleSet(fmt.Sprintf("r%d.example", i)))
	}
	spec.HostRules = types.AppendHostRuleSets(spec.HostRules)
	want := types.CloneHostRuleSets(spec.HostRules)
	allow, deny := []string{"api.example"}, []string{"denied.example"}
	off := false
	seal := []sealLayer{
		{rec: policyref.Provenance{Layer: policyref.LayerBoundary}, egress: boundarySealEgress(types.BoundarySpec{AllowedDomains: &allow, DeniedDomains: &deny})},
		restrictionSealLayer(types.Restriction{ID: uuid.New(), Name: "bounded", Spec: types.RestrictionSpec{AllowedDomains: &allow, DeniedDomains: &deny, AllowAllEgress: &off}}),
	}
	seal = append(seal, wallsSealLayer(personalProvenance, types.PersonalWallsSpec{DeniedDomains: &deny})...)
	sealPolicy(&spec, seal)
	if !reflect.DeepEqual(spec.HostRules, want) {
		t.Errorf("the seal changed the run's host rules: %+v, want %+v", spec.HostRules, want)
	}
	if !slices.Equal(spec.AllowedDomains, []string{"api.example"}) {
		t.Errorf("the seal changed the run's allowlist to %v, want [api.example]", spec.AllowedDomains)
	}
}

// TestRequestRulesCompiled: each component that carries rules adds its rules as one set of their own beside the
// policy's sets, never merged into another; a component without rules adds none, and a run without any keeps its
// sets as they were.
func TestRequestRulesCompiled(t *testing.T) {
	f := newComponentFixture(t)
	requestRulesOn(t, &f.st.siteConfig)
	orgDef := types.ComponentDefinition{Hosts: []string{"org-rules.example"}, RequestRules: []types.HostRule{
		{Host: "org-rules.example", Paths: []string{"/admin"}, Effect: types.HostRuleDeny},
		{Host: "org-rules.example", Effect: types.HostRuleAllow},
	}}
	f.org(compOrgID, orgDef)
	orgID := uuid.MustParse(compOrgID)
	a, b := rulesDefinition("a.example"), rulesDefinition("b.example")
	plain := types.ComponentDefinition{Hosts: []string{"plain.example"}}
	req := createRunRequest{Components: []types.ComponentRef{{Inline: &a}, {Inline: &plain}, {ID: &orgID}, {Inline: &b}}}
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	spec := govDeployment()
	policy := policyRuleSet("policy-rules.example")
	spec.HostRules = []types.HostRuleSet{policy}
	if _, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true); refusal != nil {
		t.Fatalf("refusal = %+v, want admitted", refusal.body)
	}
	want := types.AppendHostRuleSets([]types.HostRuleSet{policy},
		[]types.HostRuleSet{{Rules: a.RequestRules}, {Rules: orgDef.RequestRules}, {Rules: b.RequestRules}})
	if len(want) != 4 || !reflect.DeepEqual(spec.HostRules, want) {
		t.Errorf("host rules = %+v, want the policy's set and one per component with rules (%+v)", spec.HostRules, want)
	}
	for _, h := range []string{"a.example", "b.example", "org-rules.example", "plain.example"} {
		if !slices.Contains(spec.AllowedDomains, h) {
			t.Errorf("allowed_domains %v lacks %q", spec.AllowedDomains, h)
		}
	}
	// Each set decides on its own host as written.
	for _, tc := range []struct {
		host, method, path string
		want               types.HostRuleEffect
	}{
		{"a.example", http.MethodGet, "/x", types.HostRuleAllow},
		{"a.example", http.MethodPost, "/x", types.HostRuleDeny},
		{"org-rules.example", http.MethodGet, "/admin/users", types.HostRuleDeny},
		{"org-rules.example", http.MethodPost, "/v1", types.HostRuleAllow},
		{"plain.example", http.MethodDelete, "/x", types.HostRuleAllow},
	} {
		if got := types.EvalHostRules(spec.HostRules, tc.host, tc.method, tc.path).Effect; got != tc.want {
			t.Errorf("%s %s%s = %s, want %s", tc.method, tc.host, tc.path, got, tc.want)
		}
	}

	// No component with rules: the spec's sets are the very ones it had.
	spec = govDeployment()
	spec.HostRules = []types.HostRuleSet{policyRuleSet("z.example"), policyRuleSet("policy-rules.example")}
	unsorted := types.CloneHostRuleSets(spec.HostRules)
	req = createRunRequest{Components: []types.ComponentRef{{Inline: &plain}}}
	if _, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true); refusal != nil {
		t.Fatalf("no rules: refusal = %+v, want admitted", refusal.body)
	}
	if !reflect.DeepEqual(spec.HostRules, unsorted) {
		t.Errorf("no rules: host rules = %+v, want %+v untouched", spec.HostRules, unsorted)
	}
}

// TestRequestRulesSurviveRevive: a revived run keeps the host rule sets its stored config carries, a component's
// among them, whatever the switch says now; a revive re-checks only what starts a process.
func TestRequestRulesSurviveRevive(t *testing.T) {
	f, _ := newOwnerFixture(t)
	row := reviveSelfRow
	row.Definition = rulesDefinition("typed-by-the-person.example")
	f.withRunComponents(row)
	f.st.run.UserType = types.UserTypeStandard
	set := types.HostRuleSet{Rules: row.Definition.RequestRules}
	f.editConfig(t, func(c *proxy.Config) {
		c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, "typed-by-the-person.example")
		c.Policy.HostRules = []types.HostRuleSet{set}
	})
	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200 (the switch is off and a revive does not ask it)", code, body)
	}
	cfg := f.newConfig(t)
	if !reflect.DeepEqual(cfg.Policy.HostRules, []types.HostRuleSet{set}) {
		t.Errorf("revived host rules = %+v, want the component's set kept", cfg.Policy.HostRules)
	}
}

// TestRequestRulesNeverCarrySecretValues: no secret value enters a refusal or an audit row. A component with an operator's secret, a member's own secret and
// rules is refused at save, at each create door and at the limit, and neither secret value is in any answer or any
// audit row written for it.
func TestRequestRulesNeverCarrySecretValues(t *testing.T) {
	values := []string{"operator-value", "own-value"}
	leaks := func(t *testing.T, where string, texts ...string) {
		t.Helper()
		for _, text := range texts {
			for _, v := range values {
				if strings.Contains(text, v) {
					t.Errorf("%s carries the secret value %q: %s", where, v, text)
				}
			}
		}
	}
	auditTexts := func(evs []types.AuditEvent) []string {
		var out []string
		for _, ev := range evs {
			out = append(out, ev.Action+" "+ev.Target+" "+string(ev.Data))
		}
		return out
	}

	// Save: a member's component with their own secret, and an organisation's with the operator's shared one.
	srv, _, rec := requestRulesSaveServer(t, false)
	memberDef := `{"hosts":["rules.example"],"secrets":[{"secret_name":"` + compOwnSecret + `","delivery":{"mode":"header","host":"rules.example"}}],` +
		`"request_rules":[{"host":"rules.example","effect":"allow"}]}`
	orgDef := `{"hosts":["rules.example"],"secrets":[{"secret_name":"` + compOperatorSecret + `","shared":true,"delivery":{"mode":"header","host":"rules.example"}}],` +
		`"request_rules":[{"host":"rules.example","effect":"allow"}]}`
	for _, tc := range []struct {
		method, path string
		cookie       *http.Cookie
		def          string
	}{
		{http.MethodPost, "/api/v1/me/components", ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser), memberDef},
		{http.MethodPut, "/api/v1/components/" + uuid.NewString(), ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin), orgDef},
	} {
		w := doSSO(t, srv, tc.method, tc.path, tc.cookie, saveComponentBody("Rules", tc.def))
		if errorReason(w) != reasonComponentRequestRulesOff {
			t.Fatalf("%s %s = %d %s, want %s", tc.method, tc.path, w.Code, w.Body.String(), reasonComponentRequestRulesOff)
		}
		leaks(t, "the save refusal", w.Body.String())
	}
	leaks(t, "an audit row of the save doors", auditTexts(rec.snapshot())...)

	// Create, off: the member's own secret inline, and the operator's on the organisation's row.
	f := newComponentFixture(t)
	org := types.ComponentDefinition{Hosts: []string{"org-rules.example"}, RequestRules: rulesDefinition("org-rules.example").RequestRules,
		Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-rules.example"}}}}
	body := componentBody(rulesComponent(rulesHost, headerSecret(compOwnSecret, rulesHost)), f.org(compOrgID, org))
	for _, door := range componentDoors {
		w := f.ask(t, door, body)
		if errorReason(w) != reasonComponentRequestRulesOff {
			t.Fatalf("%s = %d %s, want %s", door, w.Code, w.Body.String(), reasonComponentRequestRulesOff)
		}
		leaks(t, door+"'s refusal", w.Body.String())
	}

	// Create, on, at the limit: sixteen components carrying the member's own secret, and the policy's set.
	requestRulesOn(t, &f.st.siteConfig)
	f.srv.cfg.DefaultPolicy.HostRules = []types.HostRuleSet{policyRuleSet("policy-rules.example")}
	refs := make([]any, types.MaxComponentRefs)
	for i := range refs {
		h := fmt.Sprintf("r%d.example", i)
		refs[i] = rulesComponent(h, envSecret(compOwnSecret, fmt.Sprintf("TOKEN_%d", i)))
	}
	for _, door := range componentDoors {
		w := f.ask(t, door, componentBody(refs...))
		if errorReason(w) != reasonComponentRequestRulesLimit {
			t.Fatalf("%s = %d %s, want %s", door, w.Code, w.Body.String(), reasonComponentRequestRulesLimit)
		}
		leaks(t, door+"'s limit refusal", w.Body.String())
	}
	leaks(t, "an audit row of the run doors", auditTexts(f.rec.snapshot())...)
}
