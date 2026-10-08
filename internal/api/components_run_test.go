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
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentTestStore is govEscapeStore plus the component seam: the two
// methods the gate and the create door use, and the one read a workspace_id
// launch needs. The embedded ComponentStore is nil — any other method of the
// seam being called is a test failure worth a panic.
type componentTestStore struct {
	*govEscapeStore
	store.ComponentStore
	cmu        sync.Mutex
	components []types.Component
	snapshots  map[uuid.UUID][]types.RunComponent
	ws         *types.Workspace
}

func (s *componentTestStore) ListComponentsByIDs(_ context.Context, owner string, ids []uuid.UUID) ([]types.Component, error) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	var out []types.Component
	for _, c := range s.components {
		if slices.Contains(ids, c.ID) && (c.Owner == "" || c.Owner == owner) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *componentTestStore) PutRunComponents(_ context.Context, runID uuid.UUID, comps []types.RunComponent) error {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if len(comps) == 0 {
		return store.ErrConflict // the real store refuses an empty snapshot
	}
	s.snapshots[runID] = comps
	return nil
}

func (s *componentTestStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	if s.ws == nil || s.ws.ID != id {
		return types.Workspace{}, store.ErrNotFound
	}
	return *s.ws, nil
}

const (
	// compOwnSecret is a secret the launching member holds in their own
	// namespace; compOperatorSecret one only the operator holds.
	compOwnSecret      = "person-secret"
	compOperatorSecret = "operator-only-secret"
)

type componentFixture struct {
	srv *Server
	st  *componentTestStore
	cs  *capStore
	rec *recRecorder
}

func newComponentFixture(t *testing.T) *componentFixture {
	t.Helper()
	cs := &capStore{}
	h := newHarness(t)
	st := &componentTestStore{govEscapeStore: newGovEscapeStore(cs), snapshots: map[uuid.UUID][]types.RunComponent{}}
	rec := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit, cfg.Broker, cfg.Runner = rec, h.broker, &fakeRunner{}
	cfg.Secrets = &memSecrets{
		m:     map[string][]byte{compOperatorSecret: []byte("operator-value"), govCorpSecret: []byte("v")},
		owned: map[string]map[string][]byte{capSub: {compOwnSecret: []byte("own-value")}},
	}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return &componentFixture{srv: New(cfg), st: st, cs: cs, rec: rec}
}

// componentDoors is the three run doors, launch first.
var componentDoors = []string{"/api/v1/runs", "/api/v1/runs/preflight", "/api/v1/runs/policy-preview"}

// ask POSTs body to one door as the signed-in member.
func (f *componentFixture) ask(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return kernelLaunch(t, f.srv, f.st.govEscapeStore, kernelUserTier, []string{"eng"}, path, string(raw))
}

// org stores an organisation's component and returns its reference.
func (f *componentFixture) org(id string, def types.ComponentDefinition) map[string]any {
	f.st.components = append(f.st.components, types.Component{ID: uuid.MustParse(id), Name: "Org Tool", Version: 3, Definition: def})
	return map[string]any{"id": id}
}

func componentBody(refs ...any) map[string]any {
	return map[string]any{"agent": "claude-code", "task": "components", "components": refs}
}

func inlineComponent(hosts []string, secrets ...map[string]any) map[string]any {
	def := map[string]any{"hosts": hosts}
	if len(secrets) > 0 {
		def["secrets"] = secrets
	}
	return map[string]any{"inline": def}
}

func headerSecret(name, host string) map[string]any {
	return map[string]any{"secret_name": name, "delivery": map[string]any{"mode": "header", "host": host}}
}

func envSecret(name, v string) map[string]any {
	return map[string]any{"secret_name": name, "delivery": map[string]any{"mode": "env", "var": v}}
}

func decodeErrorBody(t *testing.T, w *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%d body is not an error envelope: %v (%s)", w.Code, err, w.Body.String())
	}
	return got
}

// componentAudit is the data of every row of action recorded since before.
func (f *componentFixture) componentAudit(before int, action string) []string {
	var out []string
	for _, ev := range f.rec.snapshot()[before:] {
		if ev.Action == action {
			out = append(out, ev.Target+" "+string(ev.Data))
		}
	}
	return out
}

// A run that names no components is the run it always was: the gate reads
// nothing and writes nothing, and the run.create row has no component keys.
func TestRunComponents_NoneIsToday(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"absent": {"agent": "claude-code", "task": "components"},
		"empty":  componentBody(),
	} {
		t.Run(name, func(t *testing.T) {
			f := newComponentFixture(t)
			for _, door := range componentDoors {
				if w := f.ask(t, door, body); w.Code >= 300 {
					t.Fatalf("%s = %d %s, want success", door, w.Code, w.Body.String())
				}
			}
			if len(f.st.snapshots) != 0 {
				t.Errorf("run_components rows = %v, want none", f.st.snapshots)
			}
			for _, ev := range f.rec.snapshot() {
				if strings.HasPrefix(ev.Action, "run.component.") || ev.Action == "run.create" && strings.Contains(string(ev.Data), "component") {
					t.Errorf("%s row mentions components: %s", ev.Action, ev.Data)
				}
			}
		})
	}
	// The gate reads nothing for such a run: with no store at all it answers
	// the zero value, which floors and caps nothing.
	spec := govDeployment()
	comps, refusal := (&Server{}).applyRunComponents(httptest.NewRequest(http.MethodPost, componentDoors[0], nil),
		createRunRequest{}, &spec, governanceCeiling{}, nil, true)
	if refusal != nil || len(comps.attached) != 0 || comps.selfDefined != 0 || componentAutonomyCap(comps) != "" {
		t.Errorf("no components: comps = %+v, refusal = %+v, want the zero value", comps, refusal)
	}
	if got := confinementFloorSpec(spec, comps); len(got.AllowedDomains) != len(spec.AllowedDomains) || len(got.EligibleGrants) != 0 {
		t.Errorf("no components: the floor's spec = %+v, want the run's own", got)
	}
}

// The happy path at launch: a person's own component and an organisation's
// are expanded into the primitives policy already enforces, snapshotted, and
// audited — the person's by position and counts only (no name, host, header or
// secret name reaches an append-only row), the organisation's by name.
func TestRunComponents_AttachExpandsRecordsAndAudits(t *testing.T) {
	f := newComponentFixture(t)
	orgRef := f.org(compOrgID, types.ComponentDefinition{
		Hosts: []string{"org-api.example"},
		Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}},
	})
	person := map[string]any{"name": "Person Tool", "inline": map[string]any{
		"hosts": []string{"person-api.example", "person-files.example:8443"},
		"secrets": []any{
			map[string]any{"secret_name": compOwnSecret, "delivery": map[string]any{"mode": "header", "host": "person-api.example", "header": "X-Person-Token", "format": "%s"}},
			envSecret(compOwnSecret, "PERSON_TOKEN"),
			map[string]any{"secret_name": compOwnSecret, "delivery": map[string]any{"mode": "file", "file": "person-token"}},
		},
		"config": map[string]string{"PERSON_REGION": "person-eu"},
	}}
	w := f.ask(t, componentDoors[0], componentBody(person, orgRef))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Snapshot: the run's authorization record, in request order.
	snap := f.st.snapshots[created.ID]
	if len(snap) != 2 {
		t.Fatalf("snapshot = %+v, want 2 rows", snap)
	}
	if p := snap[0]; !p.SelfDefined || p.Owner != capSub || p.ComponentID != nil || p.Name != "Person Tool" || len(p.Definition.Hosts) != 2 {
		t.Errorf("person row = %+v", p)
	}
	if o := snap[1]; o.SelfDefined || o.Owner != "" || o.ComponentID == nil || o.ComponentID.String() != compOrgID || o.Version != 3 {
		t.Errorf("org row = %+v", o)
	}

	// Grants: a person's secret is owner_only whatever its delivery; the
	// organisation's shared one is not, and is marked for the operator's row.
	type grantShape struct {
		kind      types.GrantKind
		ownerOnly bool
		scope     string
	}
	var got []grantShape
	f.st.mu.Lock()
	for _, g := range f.st.grants {
		got = append(got, grantShape{g.Spec.Kind, g.Spec.OwnerOnly, string(g.Spec.Scope)})
	}
	f.st.mu.Unlock()
	want := []grantShape{
		{types.GrantAPIKey, true, `{"format":"%s","header":"X-Person-Token","host":"person-api.example","require_tls":true,"secret_name":"person-secret"}`},
		{types.GrantEnvSecret, true, `{"name":"PERSON_TOKEN","secret_name":"person-secret"}`},
		{types.GrantFileSecret, true, `{"file":"person-token","secret_name":"person-secret"}`},
		{types.GrantAPIKey, false, `{"host":"org-api.example","require_tls":true,"secret_name":"operator-only-secret","shared":true}`},
	}
	// Dispatch, already running, may add grants of its own after these.
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("grants =\n %+v\nwant\n %+v", got, want)
	}
	for _, g := range want {
		if g.kind == types.GrantAPIKey {
			if _, err := injectionRuleFromScope(json.RawMessage(g.scope)); err != nil {
				t.Errorf("the sink's strict decode refuses %s: %v", g.scope, err)
			}
		}
	}

	// Dispatch reads the expanded spec: the hosts are on the run's allowlist.
	ev := waitForRecAudit(t, f.rec, created.ID, "run.policy.resolve", "success")
	var spec types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &spec); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"person-api.example", "person-files.example:8443", "org-api.example"} {
		if !slices.Contains(spec.AllowedDomains, h) {
			t.Errorf("allowed_domains %v lacks %q", spec.AllowedDomains, h)
		}
	}

	// Audit. The rows the gate writes, in full.
	wantEntry := []string{
		`{"config_keys":1,"hosts":2,"kind":"custom","ordinal":0,"secrets":[{"delivery":"header","shared":false},{"delivery":"env","shared":false},{"delivery":"file","shared":false}],"source":"inline"}`,
		`{"component_id":"` + compOrgID + `","config_keys":0,"hosts":1,"kind":"custom","name":"Org Tool","ordinal":1,"secrets":[{"delivery":"header","shared":true}],"source":"org","version":3}`,
	}
	attach := f.componentAudit(0, "run.component.attach")
	for i, row := range attach {
		attach[i] = strings.TrimPrefix(row, created.ID.String()+" ")
	}
	if !slices.Equal(attach, wantEntry) {
		t.Errorf("run.component.attach =\n %v\nwant\n %v", attach, wantEntry)
	}
	var createRow struct {
		Components     []json.RawMessage `json:"components"`
		SelfAddedReach *bool             `json:"self_added_reach"`
	}
	for _, e := range f.rec.snapshot() {
		if e.Action == "run.create" && e.Outcome == "success" {
			if err := json.Unmarshal(e.Data, &createRow); err != nil {
				t.Fatal(err)
			}
		}
	}
	if createRow.SelfAddedReach == nil || !*createRow.SelfAddedReach || len(createRow.Components) != 2 ||
		string(createRow.Components[0]) != wantEntry[0] || string(createRow.Components[1]) != wantEntry[1] {
		t.Errorf("run.create components = %s, self_added_reach = %v", createRow.Components, createRow.SelfAddedReach)
	}
	egress := f.componentAudit(0, "run.egress.add")
	egress = slices.DeleteFunc(egress, func(row string) bool { return !strings.Contains(row, `"kind":"component"`) })
	wantEgress := []string{
		created.ID.String() + ` {"added_count":2,"kind":"component","ordinal":0,"source":"inline"}`,
		created.ID.String() + ` {"added_count":1,"added_domains":["org-api.example"],"kind":"component","ordinal":1,"source":"org"}`,
	}
	if !slices.Equal(egress, wantEgress) {
		t.Errorf("run.egress.add =\n %v\nwant\n %v", egress, wantEgress)
	}
	// Nothing a person typed into their component is in any row the gate wrote.
	for _, e := range f.rec.snapshot() {
		switch e.Action {
		case "run.create", "run.component.attach", "run.component.refuse", "run.egress.add", "authz.denied":
		default:
			continue
		}
		for _, typed := range []string{"person-api", "person-files", "X-Person-Token", "person-secret", "Person Tool", "PERSON_TOKEN", "person-token", "PERSON_REGION", "person-eu"} {
			if strings.Contains(string(e.Data), typed) || strings.Contains(e.Target, typed) {
				t.Errorf("%s row carries the person's %q: %s", e.Action, typed, e.Data)
			}
		}
	}
}

// What the gate hands the stages after it: the hosts dispatch must intercept,
// the plain environment, the count the autonomy cap reads, and — on the
// preview alone — an organisation's secret that is not stored yet.
func TestRunComponents_GateRecordsWhatLaterStagesRead(t *testing.T) {
	f := newComponentFixture(t)
	f.st.siteConfig.Components = &types.ComponentSettings{AutonomyCap: types.AutonomyL1}
	orgID := uuid.MustParse(compOrgID)
	f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{{
		SecretName: "not-stored", Shared: true, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}})
	req := createRunRequest{Components: []types.ComponentRef{
		{Inline: &types.ComponentDefinition{
			Hosts:   []string{"svc.example", "api.anthropic.com.evil.example"},
			Secrets: []types.ComponentSecret{{SecretName: compOwnSecret, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "svc.example"}}},
			Config:  map[string]string{"REGION": "eu"},
		}},
		{ID: &orgID},
	}}
	r := httptest.NewRequest(http.MethodPost, componentDoors[2], nil).WithContext(memberCtx([]string{"eng"}))
	spec := govDeployment()
	comps, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, false)
	if refusal != nil {
		t.Fatalf("preview refused: %+v", refusal.body)
	}
	if comps.selfDefined != 1 || componentAutonomyCap(comps) != types.AutonomyL1 || len(comps.attached) != 2 {
		t.Errorf("comps = %+v", comps)
	}
	if !slices.Equal(comps.mitmHosts, []string{"svc.example", "org-api.example"}) || len(comps.configEnv) != 1 || comps.configEnv["REGION"] != "eu" {
		t.Errorf("mitmHosts = %v, configEnv = %v", comps.mitmHosts, comps.configEnv)
	}
	if own, org := comps.attached[0], comps.attached[1]; own.source != componentSourceInline || own.needsAdminSecret ||
		!slices.Equal(own.addedHosts, []string{"svc.example", "api.anthropic.com.evil.example"}) ||
		org.source != componentSourceOrg || !org.needsAdminSecret || !slices.Equal(org.addedHosts, []string{"org-api.example"}) {
		t.Errorf("attached = %+v", comps.attached)
	}
	// The other two doors refuse the same request for the missing secret.
	spec = govDeployment()
	if _, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true); refusal == nil ||
		refusal.body.Reason != reasonComponentSecretMissing || len(spec.EligibleGrants) != 0 {
		t.Errorf("launch: refusal = %+v, grants = %v, want component_secret_missing and an untouched spec", refusal, spec.EligibleGrants)
	}
}

// The dry doors expand the same way, and a member's view of the spec never
// carries a secret name.
func TestRunComponents_DryDoorsAdmitWhatLaunchAdmits(t *testing.T) {
	f := newComponentFixture(t)
	body := componentBody(inlineComponent([]string{"person-api.example"}, headerSecret(compOwnSecret, "person-api.example")))
	if w := f.ask(t, componentDoors[1], body); w.Code != http.StatusOK {
		t.Fatalf("preflight = %d %s, want 200", w.Code, w.Body.String())
	}
	w := f.ask(t, componentDoors[2], body)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d %s, want 200", w.Code, w.Body.String())
	}
	var preview policyPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(preview.Spec.AllowedDomains, "person-api.example") {
		t.Errorf("preview allowed_domains = %v, want the component's host", preview.Spec.AllowedDomains)
	}
	if strings.Contains(w.Body.String(), compOwnSecret) {
		t.Errorf("preview carries a secret name: %s", w.Body.String())
	}
	if len(f.st.snapshots) != 0 || len(f.componentAudit(0, "run.component.attach")) != 0 {
		t.Error("a dry door persisted or audited a component")
	}
}

type componentCase struct {
	name  string
	setup func(f *componentFixture) []any // returns the request's components
	// status and reason at launch and Review; sentence, when set, is the whole
	// error. previewAdmits: the policy preview alone answers 200.
	status        int
	reason        string
	sentence      string
	previewAdmits bool
}

func refs(r ...any) func(*componentFixture) []any {
	return func(*componentFixture) []any { return r }
}

// run asks all three doors and requires the same refusal from each — the same
// status and the same bytes — and returns that body.
func (c componentCase) run(t *testing.T) {
	t.Helper()
	f := newComponentFixture(t)
	body := componentBody(c.setup(f)...)
	var first string
	for i, door := range componentDoors {
		w := f.ask(t, door, body)
		if i == 2 && c.previewAdmits {
			if w.Code != http.StatusOK {
				t.Errorf("%s = %d %s, want 200", door, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != c.status {
			t.Errorf("%s = %d %s, want %d %s", door, w.Code, w.Body.String(), c.status, c.reason)
			continue
		}
		got := decodeErrorBody(t, w)
		if got.Reason != c.reason || c.sentence != "" && got.Error != c.sentence {
			t.Errorf("%s = %q %q, want %q %q", door, got.Reason, got.Error, c.reason, c.sentence)
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("%s answered %s, launch answered %s", door, w.Body.String(), first)
		}
	}
}

func TestRunComponents_ShapeAndStoreRefusals(t *testing.T) {
	nine := make([]any, 9)
	for i := range nine {
		nine[i] = inlineComponent([]string{"a.example"})
	}
	for _, c := range []componentCase{
		{name: "more than eight", setup: refs(nine...), status: 400, reason: "component_ref_invalid",
			sentence: "components: 9 entries exceeds the 8-component limit"},
		{name: "neither id nor inline", setup: refs(map[string]any{"name": "x"}), status: 400, reason: "component_ref_invalid",
			sentence: "components[0]: give exactly one of id and inline"},
		{name: "both id and inline", setup: refs(map[string]any{"id": compOrgID, "inline": map[string]any{"hosts": []string{}}}),
			status: 400, reason: "component_ref_invalid", sentence: "components[0]: give exactly one of id and inline"},
		{name: "a label on a stored component", setup: refs(map[string]any{"id": compOrgID, "name": "x"}), status: 400, reason: "component_ref_invalid"},
		{name: "one component twice", setup: func(f *componentFixture) []any {
			ref := f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}})
			return []any{ref, map[string]any{"id": strings.ToUpper(compOrgID)}}
		}, status: 400, reason: "component_ref_invalid", sentence: "components[1]: the same component as components[0]"},
	} {
		t.Run(c.name, c.run)
	}

	// A store that cannot record components refuses them; it never launches the
	// run without them.
	cs := &capStore{}
	srv, st, _ := govEscapeFixture(t, cs)
	body, _ := json.Marshal(componentBody(inlineComponent([]string{"a.example"})))
	for _, door := range componentDoors {
		w := kernelLaunch(t, srv, st, kernelUserTier, []string{"eng"}, door, string(body))
		if got := decodeErrorBody(t, w); w.Code != http.StatusNotImplemented || got.Reason != "component_store_unavailable" {
			t.Errorf("%s = %d %s, want 501 component_store_unavailable", door, w.Code, w.Body.String())
		}
	}
}

// Who may attach: the feature for a component of one's own, the component's
// own grant for an organisation's. Asked before anything about the content.
func TestRunComponents_WhoMayAttach(t *testing.T) {
	deny := func(kind, value string) func(f *componentFixture) {
		return func(f *componentFixture) {
			f.cs.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", kind, value, types.CapabilityDeny)}
		}
	}
	// The content is invalid in every row: the door answers first.
	bad := inlineComponent([]string{"10.0.0.1"})
	for _, c := range []componentCase{
		{name: "custom components turned off: inline", setup: func(f *componentFixture) []any {
			deny(capFeature, featureCustomComponent)(f)
			return []any{bad}
		}, status: 403, reason: "capability_feature", sentence: "Custom components aren't turned on for you. Ask your admin."},
		{name: "custom components turned off: an own saved row", setup: func(f *componentFixture) []any {
			deny(capFeature, featureCustomComponent)(f)
			f.st.components = []types.Component{{ID: uuid.MustParse(compOtherID), Owner: capSub, Name: "mine", Version: 1,
				Definition: types.ComponentDefinition{Hosts: []string{"a.example"}}}}
			return []any{map[string]any{"id": compOtherID}}
		}, status: 403, reason: "capability_feature"},
		{name: "an organisation's component nobody granted", setup: func(f *componentFixture) []any {
			f.cs.restricted = map[string]map[string]bool{capComponent: {compOrgID: true}}
			return []any{f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"10.0.0.1."}})}
		}, status: 403, reason: "capability_component", sentence: componentUnavailableRefusal},
	} {
		t.Run(c.name, c.run)
	}

	// The feature turned off does not reach an organisation's component, and a
	// granted one attaches.
	f := newComponentFixture(t)
	deny(capFeature, featureCustomComponent)(f)
	f.cs.restricted = map[string]map[string]bool{capComponent: {compOrgID: true}}
	f.cs.grants = append(f.cs.grants, grant(types.CapabilitySubjectUser, capSub, capComponent, compOrgID, types.CapabilityAllow))
	body := componentBody(f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}}))
	for _, door := range componentDoors {
		if w := f.ask(t, door, body); w.Code >= 300 {
			t.Errorf("granted org component at %s = %d %s, want success", door, w.Code, w.Body.String())
		}
	}
}

// An id that names nothing the caller may attach answers exactly as an
// organisation's component they are not granted: the same status, the same
// bytes and the same authz.denied row, at every door. Whether an id exists —
// as an organisation's row, as another person's, or not at all — is not
// observable, and neither is its spelling.
func TestRunComponents_AnAbsentIDAnswersAsAnUngrantedOne(t *testing.T) {
	const carolID = "6f0c1d2e-0000-4000-8000-0000000c0355"
	ids := map[string]string{
		"ungranted, real":              compOrgID,
		"ungranted, real, upper case":  strings.ToUpper(compOrgID),
		"ungranted, real, braced":      "{" + compOrgID + "}",
		"ungranted, real, urn":         "urn:uuid:" + compOrgID,
		"absent":                       compAbsentID,
		"another person's saved row":   carolID,
		"absent, upper case":           strings.ToUpper(compAbsentID),
		"absent under a kept restrict": compOtherID,
	}
	for _, door := range componentDoors {
		t.Run(door, func(t *testing.T) {
			f := newComponentFixture(t)
			f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}})
			f.st.components = append(f.st.components, types.Component{ID: uuid.MustParse(carolID), Owner: "sub-carol", Name: "carol's",
				Definition: types.ComponentDefinition{Hosts: []string{"carol.example"}}})
			// compOtherID: a deleted organisation component, whose restriction is kept.
			f.cs.restricted = map[string]map[string]bool{capComponent: {compOrgID: true, compOtherID: true}}
			var wantRow string
			for _, name := range sortedKeys(ids) {
				before := len(f.rec.snapshot())
				w := f.ask(t, door, componentBody(map[string]any{"id": ids[name]}))
				if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != componentRefusalBody {
					t.Errorf("%s: %d %s, want 403 %s", name, w.Code, w.Body.String(), componentRefusalBody)
				}
				rows := f.componentAudit(before, "authz.denied")
				if len(rows) != 1 {
					t.Fatalf("%s: authz.denied rows = %v, want one", name, rows)
				}
				if wantRow == "" {
					wantRow = rows[0]
				} else if rows[0] != wantRow {
					t.Errorf("%s: authz.denied row %s, want %s", name, rows[0], wantRow)
				}
				if strings.Contains(rows[0], "0000000c03") || strings.Contains(w.Body.String(), "carol") {
					t.Errorf("%s: the refusal names the component: %s / %s", name, rows[0], w.Body.String())
				}
			}
		})
	}

	// An id that is not a uuid never reaches the gate: the request body does
	// not decode. The same 400 at every door, whatever exists.
	f := newComponentFixture(t)
	var first string
	for i, door := range componentDoors {
		w := f.ask(t, door, componentBody(map[string]any{"id": "jira-api"}))
		got := decodeErrorBody(t, w)
		if w.Code != http.StatusBadRequest || got.Reason != reasonInvalidRequestBody {
			t.Errorf("%s: %d %s, want 400 %s", door, w.Code, w.Body.String(), reasonInvalidRequestBody)
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("%s answered %s, launch answered %s", door, w.Body.String(), first)
		}
	}
}

// The shape rules the gate re-runs at every door, for a stored row as for an
// inline one.
func TestRunComponents_DefinitionRefusals(t *testing.T) {
	var cases []componentCase
	// A person may not name an address in any spelling a resolver reads as one.
	for _, literal := range []string{
		"10.0.0.1", "10.0.0.1:443", "127.1", "2130706433", "0x7f.1", "0177.0.0.1", "0x7f000001", "1.2.3",
		"::1", "[::1]:443", "::ffff:10.0.0.1", "fd00::1", "*.10.0.0",
	} {
		cases = append(cases, componentCase{name: "person host " + literal, setup: refs(inlineComponent([]string{literal})),
			status: 422, reason: "component_definition_invalid"})
	}
	cases = append(cases,
		componentCase{name: "a saved row of one's own is re-checked", setup: func(f *componentFixture) []any {
			f.st.components = []types.Component{{ID: uuid.MustParse(compOtherID), Owner: capSub, Name: "mine", Version: 1,
				Definition: types.ComponentDefinition{Hosts: []string{"10.0.0.1"}}}}
			return []any{map[string]any{"id": compOtherID}}
		}, status: 422, reason: "component_definition_invalid",
			sentence: `components[0]: definition.hosts[0]: "10.0.0.1" must be a DNS name, not an IP address`},
		componentCase{name: "a header host with a port", setup: refs(inlineComponent([]string{"svc.example:8443"}, headerSecret(compOwnSecret, "svc.example:8443"))),
			status: 422, reason: "component_definition_invalid"},
		componentCase{name: "a header host listed only on another port", setup: refs(inlineComponent([]string{"svc.example:8443"}, headerSecret(compOwnSecret, "svc.example"))),
			status: 422, reason: "component_definition_invalid"},
		componentCase{name: "plain_http on a person's component", setup: refs(inlineComponent([]string{"svc.example"},
			map[string]any{"secret_name": compOwnSecret, "delivery": map[string]any{"mode": "header", "host": "svc.example", "plain_http": true}})),
			status: 422, reason: "component_definition_invalid"},
		componentCase{name: "shared on a person's component", setup: refs(inlineComponent([]string{"svc.example"},
			map[string]any{"secret_name": compOwnSecret, "shared": true, "delivery": map[string]any{"mode": "header", "host": "svc.example"}})),
			status: 422, reason: "component_definition_invalid"},
		componentCase{name: "a shared secret delivered into the sandbox", setup: func(f *componentFixture) []any {
			return []any{f.org(compOrgID, types.ComponentDefinition{Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
				Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "ORG_TOKEN"}}}})}
		}, status: 422, reason: "component_definition_invalid",
			sentence: "components[0]: this component has a definition that is not valid here. Ask your admin."},
		componentCase{name: "a model provider's variable", setup: refs(inlineComponent(nil, envSecret(compOwnSecret, "ANTHROPIC_API_KEY"))),
			status: 422, reason: "component_definition_invalid",
			sentence: `components[0]: inline.secrets[0].delivery.var: "ANTHROPIC_API_KEY" is set by the run's model provider`},
		componentCase{name: "a model provider's variable as config", setup: refs(map[string]any{"inline": map[string]any{"config": map[string]string{"OPENAI_BASE_URL": "https://x.example"}}}),
			status: 422, reason: "component_definition_invalid"},
		componentCase{name: "two components into one variable", setup: refs(
			inlineComponent(nil, envSecret(compOwnSecret, "TOKEN")), inlineComponent(nil, envSecret(compOwnSecret, "TOKEN"))),
			status: 422, reason: "component_definition_invalid",
			sentence: "components[1]: inline.secrets[0].delivery: another part of this run already delivers to the same place"},
	)
	// A name Wardyn manages is refused even when the person's namespace holds it.
	for _, name := range []string{"wardyn-harness-adoown-x-oauth", "wardyn-provider-abc-key", "wardyn-signing-key", "bedrock-api-key"} {
		for _, sec := range []map[string]any{headerSecret(name, "svc.example"), envSecret(name, "TOKEN")} {
			mode := sec["delivery"].(map[string]any)["mode"].(string)
			cases = append(cases, componentCase{name: "managed name " + name + " as " + mode, setup: func(f *componentFixture) []any {
				f.srv.cfg.Secrets.For(capSub).Put(context.Background(), name, []byte("x")) //nolint:errcheck // an in-memory store
				return []any{inlineComponent([]string{"svc.example"}, sec)}
			}, status: 422, reason: "component_definition_invalid",
				sentence: `components[0]: inline.secrets[0].secret_name: "` + name + `" is managed by Wardyn`})
		}
	}
	for _, c := range cases {
		t.Run(c.name, c.run)
	}

	// An organisation's row keeps the admin's semantics for an address.
	f := newComponentFixture(t)
	body := componentBody(f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"10.20.30.40", "10.20.30.41:8443"}}))
	for _, door := range componentDoors {
		if w := f.ask(t, door, body); w.Code >= 300 {
			t.Errorf("org literal at %s = %d %s, want success", door, w.Code, w.Body.String())
		}
	}
}

// Ownership before existence: a secret the component does not get from the
// organisation must be the caller's own, and the answer never depends on what
// the operator holds — so the gate cannot be used to learn the operator's
// secret names. A secret the organisation provides is the reverse: read from
// the operator's namespace alone, and reported without its name.
func TestRunComponents_Secrets(t *testing.T) {
	notOwned := func(name string) componentCase {
		return componentCase{name: "not the caller's own: " + name,
			setup:  refs(inlineComponent([]string{"svc.example"}, headerSecret(name, "svc.example"))),
			status: 422, reason: "component_secret_not_owned",
			sentence: `components[0]: inline.secrets[0].secret_name: you have no secret named "` + name + `" of your own. Store it under Your account first.`}
	}
	orgSecret := func(sec types.ComponentSecret) func(f *componentFixture) []any {
		return func(f *componentFixture) []any {
			sec.Delivery = types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}
			return []any{f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{sec}})}
		}
	}
	for _, c := range []componentCase{
		notOwned(compOperatorSecret), // the operator has it; the person does not
		notOwned("nobody-has-this"),  // nobody has it: the same answer but for the name
		{name: "an organisation's component with a secret of the person's own", setup: orgSecret(types.ComponentSecret{SecretName: "nobody-has-this"}),
			status: 422, reason: "component_secret_not_owned",
			sentence: `components[0]: this component needs a secret of your own named "nobody-has-this". Store it under Your account first.`},
		{name: "a shared secret the operator has not stored", setup: orgSecret(types.ComponentSecret{SecretName: "nobody-has-this", Shared: true}),
			status: 422, reason: "component_secret_missing", previewAdmits: true,
			sentence: "components[0]: this component needs a secret your admin has not provided yet. Ask your admin."},
		// The person's own secret of the same name does not stand in for the
		// operator's.
		{name: "a shared secret only the person holds", setup: orgSecret(types.ComponentSecret{SecretName: compOwnSecret, Shared: true}),
			status: 422, reason: "component_secret_missing", previewAdmits: true},
	} {
		t.Run(c.name, c.run)
	}

	f := newComponentFixture(t)
	body := componentBody(orgSecret(types.ComponentSecret{SecretName: compOperatorSecret, Shared: true})(f)...)
	for _, door := range componentDoors {
		if w := f.ask(t, door, body); w.Code >= 300 {
			t.Errorf("shared secret the operator holds, at %s = %d %s, want success", door, w.Code, w.Body.String())
		}
	}
}

// The destinations a component may not reach, identically at every door.
func TestRunComponents_HostRefusals(t *testing.T) {
	host := func(h string) func(*componentFixture) []any { return refs(inlineComponent([]string{h})) }
	var cases []componentCase
	// A model's host, on any port, in any spelling, or under a wildcard — and
	// refused for what it reaches before it is refused for how it is spelled.
	for _, h := range []string{
		"api.openai.com", "api.openai.com:443", "api.openai.com:8443", "API.OpenAI.com.", "*.openai.com", "*.com",
		"api.anthropic.com", "anthropic.com", "*.anthropic.com", "console.anthropic.com:8443",
	} {
		cases = append(cases, componentCase{name: "model host " + h, setup: host(h), status: 422, reason: "component_host_serves_model",
			sentence: `components[0]: inline.hosts[0]: "` + h + `" serves a model on this deployment, and a component may not reach a model's host`})
	}
	cases = append(cases,
		componentCase{name: "an organisation's component on a model host", setup: func(f *componentFixture) []any {
			return []any{f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"api.openai.com"}})}
		}, status: 422, reason: "component_host_serves_model",
			sentence: "components[0]: this component reaches a host that serves a model, which a component may not. Ask your admin."},
		componentCase{name: "the deployment's deny list", setup: func(f *componentFixture) []any {
			f.srv.cfg.DefaultPolicy.DeniedDomains = []string{"*.blocked.example"}
			return []any{inlineComponent([]string{"ok.example", "api.blocked.example:8443"})}
		}, status: 422, reason: "component_host_denied",
			sentence: `components[0]: inline.hosts[1]: "api.blocked.example:8443" is blocked by your organisation`},
		componentCase{name: "an egress_host deny row", setup: func(f *componentFixture) []any {
			f.cs.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "capdenied.example", types.CapabilityDeny)}
			return []any{inlineComponent([]string{"capdenied.example"})}
		}, status: 422, reason: "component_host_denied"},
		componentCase{name: "egress_host enforced with no allow", setup: func(f *componentFixture) []any {
			f.cs.enf = map[string]bool{capEgressHost: true}
			return []any{inlineComponent([]string{"unlisted.example"})}
		}, status: 422, reason: "component_host_denied"},
		componentCase{name: "an organisation's component on a denied host", setup: func(f *componentFixture) []any {
			f.srv.cfg.DefaultPolicy.DeniedDomains = []string{"blocked.example"}
			return []any{f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"blocked.example"}})}
		}, status: 422, reason: "component_host_denied",
			sentence: "components[0]: this component reaches a host that is blocked for you. Ask your admin."},
		// One credential per host: against a grant already on the run, against
		// a corporate redirect's target on another port, against another
		// component.
		componentCase{name: "a host the policy already credentials", setup: func(f *componentFixture) []any {
			f.srv.cfg.DefaultPolicy.AllowedDomains = append(f.srv.cfg.DefaultPolicy.AllowedDomains, "corp.example")
			f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey, TTLSeconds: 3600,
				Scope: json.RawMessage(`{"host":"Corp.Example","secret_name":"` + govCorpSecret + `"}`)}}
			return []any{inlineComponent([]string{"corp.example:443"}, headerSecret(compOwnSecret, "corp.example"))}
		}, status: 422, reason: "component_host_collision",
			sentence: `components[0]: inline.secrets[0].delivery.host: "corp.example" already has a credential on this run, and a host carries only one`},
		componentCase{name: "a redirect's target on another port", setup: func(f *componentFixture) []any {
			f.st.siteConfig.EgressRedirects = []types.EgressRedirect{{Ecosystem: "npm", To: "https://mirror.corp.example:8443/npm/"}}
			return []any{inlineComponent([]string{"mirror.corp.example"}, headerSecret(compOwnSecret, "mirror.corp.example"))}
		}, status: 422, reason: "component_host_collision"},
		componentCase{name: "another component's header host", setup: refs(
			inlineComponent([]string{"svc.example"}, headerSecret(compOwnSecret, "svc.example")),
			inlineComponent([]string{"svc.example:443"}, headerSecret(compOwnSecret, "svc.example"))),
			status: 422, reason: "component_host_collision",
			sentence: `components[1]: inline.secrets[0].delivery.host: "svc.example" already has a credential on this run, and a host carries only one`},
	)
	for _, c := range cases {
		t.Run(c.name, c.run)
	}

	// Reach without a credential does not collide: two components may open the
	// same host, and one may open a host another credentials.
	f := newComponentFixture(t)
	body := componentBody(
		inlineComponent([]string{"svc.example"}, headerSecret(compOwnSecret, "svc.example")),
		inlineComponent([]string{"svc.example", "*.svc.example"}))
	for _, door := range componentDoors {
		if w := f.ask(t, door, body); w.Code >= 300 {
			t.Errorf("shared reach at %s = %d %s, want success", door, w.Code, w.Body.String())
		}
	}
}

// A workspace's permanent denies bind a component at every door. Launch folds
// them into the spec only after the run exists, so the gate reads them from
// the workspaces themselves; without that, Review and the preview would refuse
// a host launch admits.
func TestRunComponents_AWorkspaceDenyBindsAtEveryDoor(t *testing.T) {
	f := newComponentFixture(t)
	ws := types.Workspace{DeniedEgress: []string{"ws-denied.example"}}
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	for _, credentials := range []bool{true, false} {
		spec := govDeployment()
		req := createRunRequest{Components: []types.ComponentRef{{Inline: &types.ComponentDefinition{Hosts: []string{"ws-denied.example"}}}}}
		_, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, []types.Workspace{ws}, credentials)
		if refusal == nil || refusal.body.Reason != reasonComponentHostDenied {
			t.Fatalf("credentials=%v: refusal = %+v, want component_host_denied", credentials, refusal)
		}
		if slices.Contains(spec.AllowedDomains, "ws-denied.example") {
			t.Error("a refused component widened the spec")
		}
		// The same request with no workspace is admitted, so the workspace's
		// deny is what refused it.
		if _, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, credentials); refusal != nil {
			t.Fatalf("credentials=%v, no workspace: refusal = %+v, want admitted", credentials, refusal.body)
		}
	}
}

// The same through the real doors, as an operator launching against a
// workspace whose owner denied a host for good.
func TestRunComponents_AWorkspaceDenyRefusesTheSameAtEveryDoor(t *testing.T) {
	f := newComponentFixture(t)
	ws := types.Workspace{
		ID: uuid.New(), Name: "ws", Status: types.WorkspaceScanned, DeniedEgress: []string{"ws-denied.example"},
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: t.TempDir()}},
	}
	f.st.ws, f.st.workspaces = &ws, []types.Workspace{ws}
	ask := func(path string, hosts ...string) *httptest.ResponseRecorder {
		body := componentBody(inlineComponent(hosts))
		body["workspace_id"] = ws.ID.String()
		raw, _ := json.Marshal(body)
		return do(t, f.srv, http.MethodPost, path, adminToken, string(raw))
	}
	var first string
	for i, door := range componentDoors {
		// The control: the same launch with a host the workspace does not deny.
		if w := ask(door, "ws-allowed.example"); w.Code >= 300 {
			t.Fatalf("%s with an allowed host = %d %s, want success", door, w.Code, w.Body.String())
		}
		w := ask(door, "ws-denied.example")
		if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != "component_host_denied" {
			t.Errorf("%s = %d %s, want 422 component_host_denied", door, w.Code, w.Body.String())
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("%s answered %s, launch answered %s", door, w.Body.String(), first)
		}
	}
}

// One organisation switch turns off delivering a secret into the sandbox, for
// an organisation's component as for a person's; a header still works.
func TestRunComponents_ResidentDeliverySwitch(t *testing.T) {
	off := func(f *componentFixture) {
		f.st.siteConfig.Components = &types.ComponentSettings{DenyResidentDelivery: true}
	}
	for _, c := range []componentCase{
		{name: "a person's env delivery", setup: func(f *componentFixture) []any {
			off(f)
			return []any{inlineComponent(nil, envSecret(compOwnSecret, "TOKEN"))}
		}, status: 422, reason: "component_resident_delivery_denied"},
		{name: "a person's file delivery", setup: func(f *componentFixture) []any {
			off(f)
			return []any{inlineComponent(nil, map[string]any{"secret_name": compOwnSecret, "delivery": map[string]any{"mode": "file", "file": "token"}})}
		}, status: 422, reason: "component_resident_delivery_denied"},
		{name: "an organisation's env delivery", setup: func(f *componentFixture) []any {
			off(f)
			return []any{f.org(compOrgID, types.ComponentDefinition{Secrets: []types.ComponentSecret{{SecretName: compOwnSecret,
				Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "TOKEN"}}}})}
		}, status: 422, reason: "component_resident_delivery_denied",
			sentence: "components[0]: this component delivers a secret into the sandbox, which your organisation has turned off. Ask your admin."},
	} {
		t.Run(c.name, c.run)
	}
	f := newComponentFixture(t)
	off(f)
	body := componentBody(inlineComponent([]string{"svc.example"}, headerSecret(compOwnSecret, "svc.example")),
		map[string]any{"inline": map[string]any{"config": map[string]string{"REGION": "eu"}}})
	for _, door := range componentDoors {
		if w := f.ask(t, door, body); w.Code >= 300 {
			t.Errorf("header and config with the switch on, at %s = %d %s, want success", door, w.Code, w.Body.String())
		}
	}
}

// The strongest-sandbox floor for a component's header credential is the
// organisation's switch, off by default; every other credential floors a run
// as it always did.
func TestRunComponents_VaultFloorIsTheOrganisationsSwitch(t *testing.T) {
	enforcedAt := func(t *testing.T, f *componentFixture) (preflight, launch types.ConfinementClass) {
		t.Helper()
		body := componentBody(inlineComponent([]string{"svc.example"}, headerSecret(compOwnSecret, "svc.example")))
		body["confinement_class"] = "CC1"
		w := f.ask(t, componentDoors[1], body)
		var pre struct {
			Class types.ConfinementClass `json:"enforced_confinement_class"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &pre); err != nil || w.Code != http.StatusOK {
			t.Fatalf("preflight = %d %s (%v)", w.Code, w.Body.String(), err)
		}
		w = f.ask(t, componentDoors[0], body)
		var run struct {
			Class types.ConfinementClass `json:"confinement_class"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("create = %d %s (%v)", w.Code, w.Body.String(), err)
		}
		if w := f.ask(t, componentDoors[2], body); w.Code != http.StatusOK {
			t.Fatalf("preview = %d %s", w.Code, w.Body.String())
		}
		return pre.Class, run.Class
	}
	f := newComponentFixture(t)
	if pre, launch := enforcedAt(t, f); pre != types.CC1 || launch != types.CC1 {
		t.Errorf("default: preflight %s, launch %s, want CC1 at both (the floor is lifted)", pre, launch)
	}
	f = newComponentFixture(t)
	f.st.siteConfig.Components = &types.ComponentSettings{RequireVaultForCredentials: true}
	if pre, launch := enforcedAt(t, f); pre != types.CC3 || launch != types.CC3 {
		t.Errorf("switch on: preflight %s, launch %s, want CC3 at both", pre, launch)
	}

	// Only a component's own credential is set aside.
	policyGrant := types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"corp.example","secret_name":"k"}`)}
	compGrant := componentGrant(types.ComponentSecret{SecretName: "k", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "svc.example"}})
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{policyGrant, compGrant}}
	comps := runComponents{mitmHosts: []string{"svc.example"}}
	if got := confinementFloorSpec(spec, comps).EligibleGrants; len(got) != 1 || string(got[0].Scope) != string(policyGrant.Scope) {
		t.Errorf("floor spec grants = %+v, want the policy's alone", got)
	}
	if len(spec.EligibleGrants) != 2 {
		t.Error("confinementFloorSpec edited the run's spec")
	}
	comps.settings.RequireVaultForCredentials = true
	if got := confinementFloorSpec(spec, comps).EligibleGrants; len(got) != 2 {
		t.Errorf("switch on: floor spec grants = %+v, want both", got)
	}
	if got := confinementFloorSpec(spec, runComponents{}).EligibleGrants; len(got) != 2 {
		t.Errorf("no components: floor spec grants = %+v, want the spec untouched", got)
	}
}

// A refused launch is audited; a refused dry run is not. The row says why and
// where in the request, never what the person typed.
func TestRunComponents_RefusalIsAuditedAtLaunchOnly(t *testing.T) {
	f := newComponentFixture(t)
	body := componentBody(inlineComponent([]string{"ok.example"}), inlineComponent([]string{"api.openai.com:8443"}))
	for i, door := range componentDoors {
		before := len(f.rec.snapshot())
		if w := f.ask(t, door, body); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d %s, want 422", door, w.Code, w.Body.String())
		}
		rows := f.componentAudit(before, "run.component.refuse")
		if i > 0 {
			if len(rows) != 0 {
				t.Errorf("%s audited a refused dry run: %v", door, rows)
			}
			continue
		}
		if want := []string{` {"ordinal":1,"reason":"component_host_serves_model","source":"inline"}`}; !slices.Equal(rows, want) {
			t.Errorf("run.component.refuse = %v, want %v", rows, want)
		}
	}
	if len(f.st.snapshots) != 0 || len(f.st.runs) != 0 {
		t.Error("a refused launch left a run or a snapshot")
	}
}

// The organisation's autonomy cap reaches both gating doors through the gate's
// own count: a self-defined component is capped, an organisation's is not.
func TestRunComponents_AutonomyCapSeesSelfDefinedOnly(t *testing.T) {
	f := newComponentFixture(t)
	f.st.siteConfig.Components = &types.ComponentSettings{AutonomyCap: types.AutonomyL0}
	own := componentBody(inlineComponent([]string{"svc.example"}))
	var first string
	for i, door := range componentDoors[:2] {
		w := f.ask(t, door, own)
		if got := decodeErrorBody(t, w); w.Code != http.StatusForbidden || got.Reason != "component_autonomy" {
			t.Fatalf("%s = %d %s, want 403 component_autonomy", door, w.Code, w.Body.String())
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("Review answered %s, launch %s", w.Body.String(), first)
		}
	}
	org := componentBody(f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}}))
	for _, door := range componentDoors {
		if w := f.ask(t, door, org); w.Code >= 300 {
			t.Errorf("org component under the cap at %s = %d %s, want success", door, w.Code, w.Body.String())
		}
	}
}

// An operator-owned run's own namespace is the operator's: the admin token
// attaches a component whose secret the operator holds, and is refused one it
// does not.
func TestRunComponents_OperatorOwnedRunReadsTheOperatorNamespace(t *testing.T) {
	f := newComponentFixture(t)
	ask := func(path, secret string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(componentBody(inlineComponent([]string{"svc.example"}, headerSecret(secret, "svc.example"))))
		return do(t, f.srv, http.MethodPost, path, adminToken, string(raw))
	}
	for _, door := range componentDoors {
		if w := ask(door, compOperatorSecret); w.Code >= 300 {
			t.Errorf("%s with the operator's secret = %d %s, want success", door, w.Code, w.Body.String())
		}
		w := ask(door, compOwnSecret) // a person's row is not the operator's
		if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != "component_secret_not_owned" {
			t.Errorf("%s with a person's secret = %d %s, want 422 component_secret_not_owned", door, w.Code, w.Body.String())
		}
	}
}
