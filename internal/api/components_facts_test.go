// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// factKeys is every field a component fact may carry. A field outside it is a
// fact nobody decided to publish.
var factKeys = []string{
	"kind", "provider", "id", "name", "version", "reason", "status", "requirements", "lane", "org", "repos",
	"hosts", "secrets", "config_keys", "self_defined", "autonomy_cap", "vault_floor", "tls_intercept", "high_risk",
	"agent", "capabilities", "capability_ceiling", "push_rules", "token_mode", "token_scopes", "repo_access", "install_url",
}

// nonAgentDoorFacts preserves the custom/Git isolation matrices while agent
// facts are checked separately through the actual SDK shape. It checks every
// fact's top-level keys, including agent facts. Each non-agent fact is the bytes
// the door wrote. It fails the test on any other status and on a fact that
// carries a field outside factKeys.
func nonAgentDoorFacts(t *testing.T, door string, w *httptest.ResponseRecorder) []string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s = %d %s, want 200", door, w.Code, w.Body.String())
	}
	var body struct {
		Components []json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v (%s)", door, err, w.Body.String())
	}
	out := []string{}
	for i, raw := range body.Components {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("%s: components[%d] is not an object: %s", door, i, raw)
		}
		if string(fields["kind"]) != `"agent"` {
			out = append(out, string(raw))
		}
		for k := range fields {
			if !slices.Contains(factKeys, k) {
				t.Errorf("%s: components[%d] carries %q, which no fact is allowed to: %s", door, i, k, raw)
			}
		}
	}
	return out
}

// dryDoors is Review and the policy preview: the two doors that answer facts.
var dryDoors = componentDoors[1:]

const (
	factsOrgConfigValue = "org-config-value"
	factsOrgFact        = `{"kind":"custom","id":"` + compOrgID + `","name":"Org Tool","version":3,"reason":"org","status":"ready","requirements":[],` +
		`"hosts":["org-api.example","org-files.example:8443"],"secrets":[{"delivery":"header","shared":true}],"config_keys":["ORG_REGION"],"tls_intercept":true}`
)

// factsOrgComponent stores the organisation's component every case below
// names: two destinations, a credential the organisation provides and one
// plain setting.
func factsOrgComponent(f *componentFixture) map[string]any {
	return f.org(compOrgID, types.ComponentDefinition{
		Hosts: []string{"org-api.example", "org-files.example:8443"},
		Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}},
		Config: map[string]string{"ORG_REGION": factsOrgConfigValue},
	})
}

// The whole member-isolation matrix, at all three doors. A person granted an
// organisation's component gets it as a fact, with its destinations as a list
// and each secret's delivery, and never the secret's name or a setting's
// value; the id is the canonical one however the request spelled it. A
// component they are not granted, one that does not exist, another person's
// and a deleted one are one answer, byte for byte, with no fact and no
// destination; an id that is not a uuid is the request's own 400.
func TestComponentFacts_GrantedUngrantedAbsentAtEveryDoor(t *testing.T) {
	const (
		carolID  = "6f0c1d2e-0000-4000-8000-0000000c0355"
		mineID   = "6f0c1d2e-0000-4000-8000-0000000c0356"
		mineFact = `{"kind":"custom","id":"` + mineID + `","name":"My Tool","version":2,"reason":"self","status":"ready","requirements":[],` +
			`"hosts":["mine.example"],"secrets":[{"delivery":"env","shared":false}],"self_defined":true,"high_risk":true}`
	)
	cases := []struct {
		name, id string
		granted  bool
		status   int    // launch's; a dry door answers 200 where launch answers 201
		reason   string // of a refusal
		fact     string // of an admitted component, at both dry doors
	}{
		{name: "granted", id: compOrgID, granted: true, status: http.StatusCreated, fact: factsOrgFact},
		{name: "granted, upper case", id: strings.ToUpper(compOrgID), granted: true, status: http.StatusCreated, fact: factsOrgFact},
		{name: "granted, braced", id: "{" + compOrgID + "}", granted: true, status: http.StatusCreated, fact: factsOrgFact},
		{name: "granted, urn", id: "urn:uuid:" + compOrgID, granted: true, status: http.StatusCreated, fact: factsOrgFact},
		{name: "own saved component", id: mineID, status: http.StatusCreated, fact: mineFact},
		{name: "ungranted", id: compOrgID, status: http.StatusForbidden, reason: "capability_component"},
		{name: "ungranted, upper case", id: strings.ToUpper(compOrgID), status: http.StatusForbidden, reason: "capability_component"},
		{name: "absent", id: compAbsentID, status: http.StatusForbidden, reason: "capability_component"},
		{name: "another person's", id: carolID, status: http.StatusForbidden, reason: "capability_component"},
		{name: "deleted, restriction kept", id: compOtherID, status: http.StatusForbidden, reason: "capability_component"},
		{name: "not a uuid", id: "jira-api", status: http.StatusBadRequest, reason: reasonInvalidRequestBody},
		{name: "not a uuid, nearly", id: compOrgID + "0", status: http.StatusBadRequest, reason: reasonInvalidRequestBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newComponentFixture(t)
			factsOrgComponent(f)
			f.st.components = append(f.st.components,
				types.Component{ID: uuid.MustParse(carolID), Owner: "sub-carol", Name: "carol's", Version: 1,
					Definition: types.ComponentDefinition{Hosts: []string{"carol.example"}}},
				types.Component{ID: uuid.MustParse(mineID), Owner: capSub, Name: "My Tool", Version: 2,
					Definition: types.ComponentDefinition{Hosts: []string{"mine.example"}, Secrets: []types.ComponentSecret{{
						SecretName: compOwnSecret, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "MINE_TOKEN"}}}}})
			f.cs.restricted = map[string]map[string]bool{capComponent: {compOrgID: true, compOtherID: true}}
			if tc.granted {
				f.cs.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capComponent, compOrgID, types.CapabilityAllow)}
			}
			var refusal string
			for i, door := range componentDoors {
				w := f.ask(t, door, componentBody(map[string]any{"id": tc.id}))
				body := w.Body.String()
				// Whatever the answer, no door hands a member the organisation's
				// secret name or the value of one of its settings; and the
				// preview names no secret the person already holds (Review's
				// checklist does: it is theirs).
				private := []string{compOperatorSecret, factsOrgConfigValue, "carol"}
				if i == 2 {
					private = append(private, compOwnSecret)
				}
				for _, p := range private {
					if strings.Contains(body, p) {
						t.Errorf("%s carries %q: %s", door, p, body)
					}
				}
				if tc.fact != "" {
					if i == 0 {
						if w.Code != tc.status {
							t.Errorf("%s = %d %s, want %d", door, w.Code, body, tc.status)
						}
						continue
					}
					if got := nonAgentDoorFacts(t, door, w); !slices.Equal(got, []string{tc.fact}) {
						t.Errorf("%s components =\n %v\nwant\n %v", door, got, []string{tc.fact})
					}
					continue
				}
				if got := decodeErrorBody(t, w); w.Code != tc.status || got.Reason != tc.reason {
					t.Errorf("%s = %d %s, want %d %s", door, w.Code, body, tc.status, tc.reason)
				}
				if tc.status == http.StatusForbidden && strings.TrimSpace(body) != componentRefusalBody {
					t.Errorf("%s = %s, want the one refusal %s", door, body, componentRefusalBody)
				}
				for _, named := range []string{"org-api", "org-files", "Org Tool", "mine.example"} {
					if strings.Contains(body, named) {
						t.Errorf("%s: the refusal carries %q: %s", door, named, body)
					}
				}
				if i == 0 {
					refusal = body
				} else if body != refusal {
					t.Errorf("%s answered %s, launch answered %s", door, body, refusal)
				}
			}
		})
	}
}

// A person's own secret that is not stored yet: the policy preview keeps its
// body and says so as a setup item on the component, the same row Review's
// checklist would show; launch and Review refuse. A secret they do hold is
// not named, and for an organisation's component the item names the secret
// the person is to store.
func TestComponentFacts_PreviewReportsAMissingOwnSecretAsASetupItem(t *testing.T) {
	const (
		missing = `{"kind":"secret","id":"secret:not-stored-yet","label":"Secret: not-stored-yet","required_by":"an env_secret grant (PERSON_TOKEN)",` +
			`"status":"missing","fix":{"action":"add_secret","secret_name":"not-stored-yet"},"residency":"resident_env"}`
		teamToken = `{"kind":"secret","id":"secret:team-token","label":"Secret: team-token","required_by":"an api_key grant (org-api.example)",` +
			`"status":"missing","fix":{"action":"add_secret","secret_name":"team-token"},"residency":"proxy_injected"}`
	)
	for _, tc := range []struct {
		name string
		ref  func(f *componentFixture) any
		fact string
	}{
		{name: "defined on the request", ref: func(*componentFixture) any {
			return map[string]any{"name": "Person Tool", "inline": map[string]any{
				"hosts": []string{"person-api.example"},
				"secrets": []any{headerSecret(compOwnSecret, "person-api.example"),
					envSecret("not-stored-yet", "PERSON_TOKEN"),
					map[string]any{"secret_name": "not-stored-yet", "delivery": map[string]any{"mode": "file", "file": "person-token"}}},
			}}
		}, fact: `{"kind":"custom","id":"inline:0","name":"Person Tool","reason":"inline","status":"needs_input","requirements":[` + missing + `],` +
			`"hosts":["person-api.example"],"secrets":[{"delivery":"header","shared":false},{"delivery":"env","shared":false},{"delivery":"file","shared":false}],` +
			`"self_defined":true,"tls_intercept":true,"high_risk":true}`},
		{name: "an organisation's component", ref: func(f *componentFixture) any {
			return f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{{
				SecretName: "team-token", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}})
		}, fact: `{"kind":"custom","id":"` + compOrgID + `","name":"Org Tool","version":3,"reason":"org","status":"needs_input","requirements":[` + teamToken + `],` +
			`"hosts":["org-api.example"],"secrets":[{"delivery":"header","shared":false}],"tls_intercept":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComponentFixture(t)
			body := componentBody(tc.ref(f))
			w := f.ask(t, componentDoors[2], body)
			if got := nonAgentDoorFacts(t, componentDoors[2], w); !slices.Equal(got, []string{tc.fact}) {
				t.Errorf("preview components =\n %v\nwant\n %v", got, []string{tc.fact})
			}
			if strings.Contains(w.Body.String(), compOwnSecret) {
				t.Errorf("the preview names a secret the person already holds: %s", w.Body.String())
			}
			var first string
			for i, door := range componentDoors[:2] {
				w := f.ask(t, door, body)
				if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != reasonComponentSecretNotOwned {
					t.Errorf("%s = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonComponentSecretNotOwned)
				}
				if i == 0 {
					first = w.Body.String()
				} else if w.Body.String() != first {
					t.Errorf("%s answered %s, launch answered %s", door, w.Body.String(), first)
				}
			}
		})
	}
}

// A secret the organisation provides and has not stored: the preview says the
// component is unavailable, with nothing for the person to add and no name;
// launch and Review refuse without the name either. And a setting's value is
// in no body, the person's own included — a fact lists the keys.
func TestComponentFacts_NoDoorNamesASharedSecretOrAConfigValue(t *testing.T) {
	const notStored = "org-shared-not-stored"
	f := newComponentFixture(t)
	org := f.org(compOrgID, types.ComponentDefinition{
		Hosts: []string{"org-api.example"},
		Secrets: []types.ComponentSecret{{SecretName: notStored, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example", PlainHTTP: true}}},
		Config: map[string]string{"ORG_REGION": factsOrgConfigValue},
	})
	w := f.ask(t, componentDoors[2], componentBody(org))
	want := `{"kind":"custom","id":"` + compOrgID + `","name":"Org Tool","version":3,"reason":"org","status":"unavailable","requirements":[],` +
		`"hosts":["org-api.example"],"secrets":[{"delivery":"header","shared":true}],"config_keys":["ORG_REGION"]}`
	if got := nonAgentDoorFacts(t, componentDoors[2], w); !slices.Equal(got, []string{want}) {
		t.Errorf("preview components =\n %v\nwant\n %v", got, []string{want})
	}
	bodies := []string{w.Body.String()}
	for _, door := range componentDoors[:2] {
		w := f.ask(t, door, componentBody(org))
		if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != reasonComponentSecretMissing {
			t.Errorf("%s = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonComponentSecretMissing)
		}
		bodies = append(bodies, w.Body.String())
	}

	// The person's own settings: keys, never values, at both dry doors.
	own := map[string]any{"inline": map[string]any{"hosts": []string{"person-api.example"},
		"config": map[string]string{"PERSON_REGION": "person-config-value"}}}
	ownFact := `{"kind":"custom","id":"inline:0","reason":"inline","status":"ready","requirements":[],` +
		`"hosts":["person-api.example"],"config_keys":["PERSON_REGION"],"self_defined":true}`
	for _, door := range dryDoors {
		w := f.ask(t, door, componentBody(own))
		if got := nonAgentDoorFacts(t, door, w); !slices.Equal(got, []string{ownFact}) {
			t.Errorf("%s components =\n %v\nwant\n %v", door, got, []string{ownFact})
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, body := range bodies {
		for _, private := range []string{notStored, factsOrgConfigValue, "person-config-value"} {
			if strings.Contains(body, private) {
				t.Errorf("a door carries %q: %s", private, body)
			}
		}
	}
}

// What a fact says about the run beyond the component's own content: that the
// launcher defined it, the organisation's cap on such a run, the strongest
// sandbox where the organisation asks for it, that a connection is opened to
// add a header, and that Review grades a secret inside the sandbox high.
func TestComponentFacts_FlagsFollowTheRunAndTheOrganisationsSettings(t *testing.T) {
	type flags struct {
		SelfDefined  bool   `json:"self_defined"`
		AutonomyCap  string `json:"autonomy_cap"`
		VaultFloor   bool   `json:"vault_floor"`
		TLSIntercept bool   `json:"tls_intercept"`
		HighRisk     bool   `json:"high_risk"`
	}
	header := inlineComponent([]string{"person-api.example"}, headerSecret(compOwnSecret, "person-api.example"))
	resident := inlineComponent([]string{"person-api.example"}, envSecret(compOwnSecret, "PERSON_TOKEN"))
	baseline := inlineComponent([]string{"pypi.org"}, headerSecret(compOwnSecret, "pypi.org"))
	orgPlain := func(f *componentFixture) any {
		return f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{{
			SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example", PlainHTTP: true}}}})
	}
	orgTLS := func(f *componentFixture) any {
		return f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{{
			SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}})
	}
	fixed := func(ref any) func(*componentFixture) any { return func(*componentFixture) any { return ref } }
	strict := &types.ComponentSettings{AutonomyCap: types.AutonomyL1, RequireVaultForCredentials: true}
	for _, tc := range []struct {
		name     string
		settings *types.ComponentSettings
		ref      func(*componentFixture) any
		want     flags
	}{
		{"own header, nothing set", nil, fixed(header), flags{SelfDefined: true, TLSIntercept: true}},
		{"own header, cap and floor set", strict, fixed(header), flags{SelfDefined: true, AutonomyCap: "L1", VaultFloor: true, TLSIntercept: true}},
		{"own header to a baseline host, floor set", strict, fixed(baseline), flags{SelfDefined: true, AutonomyCap: "L1", TLSIntercept: true}},
		{"own variable", nil, fixed(resident), flags{SelfDefined: true, HighRisk: true}},
		{"organisation's header, cap and floor set", strict, orgTLS, flags{VaultFloor: true, TLSIntercept: true}},
		{"organisation's header over plain HTTP", nil, orgPlain, flags{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComponentFixture(t)
			f.st.siteConfig.Components = tc.settings
			facts := nonAgentDoorFacts(t, componentDoors[2], f.ask(t, componentDoors[2], componentBody(tc.ref(f))))
			if len(facts) != 1 {
				t.Fatalf("components = %v, want one", facts)
			}
			var got flags
			if err := json.Unmarshal([]byte(facts[0]), &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("flags = %+v, want %+v (%s)", got, tc.want, facts[0])
			}
		})
	}
}

// An exec run has no agent process. With no component or repository, neither
// dry door grows a components key.
func TestComponentFacts_NoneIsToday(t *testing.T) {
	f := newComponentFixture(t)
	for _, door := range dryDoors {
		w := f.ask(t, door, map[string]any{"agent": "claude-code", "task": "echo ok", "task_mode": "exec"})
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"components"`) {
			t.Errorf("%s = %d %s, want a 200 with no components key", door, w.Code, w.Body.String())
		}
	}
}

// factsViewStore is componentTestStore plus the one read GET /runs/{id}/policy
// makes that it lacks: the run's audit rows, from the recorder they land in.
type factsViewStore struct {
	*componentTestStore
	audit *recRecorder
}

func (s *factsViewStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	var out []types.AuditEvent
	for _, ev := range s.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// A granted person sees an organisation's component's destinations wherever
// they see any other host of their run: the preview's allowed_domains, the
// run's own policy view, and its explanation of what launch added. The
// credential shows as a grant to its host; its secret's name shows nowhere.
func TestComponentFacts_AGrantedComponentsHostsShowAsAnyRunHost(t *testing.T) {
	cs := &capStore{}
	h := newHarness(t)
	st := &componentTestStore{govEscapeStore: newGovEscapeStore(cs), snapshots: map[uuid.UUID][]types.RunComponent{}}
	rec := &recRecorder{}
	cfg := baseTestConfig(h, &factsViewStore{componentTestStore: st, audit: rec})
	cfg.Audit, cfg.Broker, cfg.Runner = rec, h.broker, &fakeRunner{}
	cfg.Secrets = &memSecrets{
		m:     map[string][]byte{compOperatorSecret: []byte("operator-value"), govCorpSecret: []byte("v")},
		owned: map[string]map[string][]byte{capSub: {compOwnSecret: []byte("own-value")}},
	}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	f := &componentFixture{srv: New(cfg), st: st, cs: cs, rec: rec}
	body := componentBody(factsOrgComponent(f))
	cs.restricted = map[string]map[string]bool{capComponent: {compOrgID: true}}
	cs.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capComponent, compOrgID, types.CapabilityAllow)}
	hosts := []string{"org-api.example", "org-files.example:8443"}

	var preview struct {
		Spec struct {
			AllowedDomains []string `json:"allowed_domains"`
		} `json:"spec"`
	}
	w := f.ask(t, componentDoors[2], body)
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || w.Code != http.StatusOK {
		t.Fatalf("preview = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	for _, host := range hosts {
		if !slices.Contains(preview.Spec.AllowedDomains, host) {
			t.Errorf("preview allowed_domains %v lacks %q", preview.Spec.AllowedDomains, host)
		}
	}

	w = f.ask(t, componentDoors[0], body)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	waitForRecAudit(t, rec, created.ID, "run.policy.resolve", "success")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+created.ID.String()+"/policy", nil)
	kernelUserTier.auth(t, st.govEscapeStore, []string{"eng"}, r)
	w = httptest.NewRecorder()
	panicFails(t, f.srv.Handler()).ServeHTTP(w, r)
	var view struct {
		Spec struct {
			AllowedDomains []string `json:"allowed_domains"`
		} `json:"spec"`
		Changes []struct {
			Field string   `json:"field"`
			Added []string `json:"added"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != http.StatusOK {
		t.Fatalf("run policy view = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	explained := map[string][]string{}
	for _, c := range view.Changes {
		explained[c.Field] = append(explained[c.Field], c.Added...)
	}
	for _, host := range hosts {
		if !slices.Contains(view.Spec.AllowedDomains, host) {
			t.Errorf("run policy view allowed_domains %v lacks %q", view.Spec.AllowedDomains, host)
		}
		if !slices.Contains(explained["allowed_domains"], host) {
			t.Errorf("the explanation's added allowed_domains %v lack %q", explained["allowed_domains"], host)
		}
	}
	if !slices.Contains(explained["eligible_grants"], "api_key:org-api.example") {
		t.Errorf("the explanation's added grants %v lack the component's credential", explained["eligible_grants"])
	}
	for _, private := range []string{compOperatorSecret, factsOrgConfigValue} {
		if strings.Contains(w.Body.String(), private) {
			t.Errorf("the run policy view carries %q: %s", private, w.Body.String())
		}
	}
}

// gitFact is the part of a git_provider fact the cases below compare.
type gitFact struct {
	Kind     string   `json:"kind"`
	Provider string   `json:"provider"`
	ID       string   `json:"id"`
	Reason   string   `json:"reason"`
	Status   string   `json:"status"`
	Lane     string   `json:"lane"`
	Org      string   `json:"org"`
	Repos    []string `json:"repos"`
}

func gitFacts(t *testing.T, door string, w *httptest.ResponseRecorder) []gitFact {
	t.Helper()
	var out []gitFact
	for _, raw := range nonAgentDoorFacts(t, door, w) {
		var fact gitFact
		if err := json.Unmarshal([]byte(raw), &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == "git_provider" {
			fact.Kind = ""
			out = append(out, fact)
		}
	}
	return out
}

// The Git provider a run's repositories live on is a fact too, one per
// provider the draft's repositories classify to and none for the other: with
// the lane the clone will take, and — for a per-person Azure DevOps
// connection, at Review — whether the person is connected. No row id.
func TestComponentFacts_GitProviderRows(t *testing.T) {
	const ghRepo = "https://github.com/acme/one"
	github := func(lane string) gitFact {
		return gitFact{Provider: "github", ID: "git_provider:github:" + lane + ":acme", Reason: "workspace", Status: "unknown", Lane: lane, Org: "acme", Repos: []string{ghRepo}}
	}
	ado := func(status string) gitFact {
		return gitFact{Provider: "azure_devops", ID: "git_provider:azure_devops:entra:" + scmTestADOOrg, Reason: "workspace", Status: status,
			Lane: "entra", Org: scmTestADOOrg, Repos: []string{scmTestADORepo}}
	}
	token := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/one"],"permissions":{"contents":"read"}}`)}
	narrow := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	withToken := narrow
	withToken.EligibleGrants = []types.GrantSpec{token}
	open := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true}
	for _, tc := range []struct {
		name      string
		policy    types.RunPolicySpec
		lanes     []types.GitLane // of the GitHub row
		repos     []string
		connected bool
		mechanism bool // asked with the shared admin token: no person to connect
		preview   []gitFact
		review    []gitFact
	}{
		{name: "GitHub alone, through the broker", policy: withToken, repos: []string{ghRepo},
			preview: []gitFact{github("app")}, review: []gitFact{github("app")}},
		{name: "GitHub alone, a direct clone", policy: open, repos: []string{ghRepo},
			preview: []gitFact{github("direct")}, review: []gitFact{github("direct")}},
		{name: "GitHub alone, nothing reaches it", policy: narrow, repos: []string{ghRepo},
			preview: []gitFact{github("none")}, review: []gitFact{github("none")}},
		{name: "GitHub alone, the row refuses the broker lane", policy: withToken, lanes: []types.GitLane{types.GitLanePAT}, repos: []string{ghRepo},
			preview: []gitFact{github("none")}, review: []gitFact{github("none")}},
		{name: "Azure DevOps alone, not connected", policy: open, repos: []string{scmTestADORepo},
			preview: []gitFact{ado("unknown")}, review: []gitFact{ado("needs_input")}},
		{name: "Azure DevOps alone, connected", policy: open, repos: []string{scmTestADORepo}, connected: true,
			preview: []gitFact{ado("unknown")}, review: []gitFact{ado("ready")}},
		{name: "Azure DevOps alone, a caller that is no person", policy: open, repos: []string{scmTestADORepo}, mechanism: true,
			preview: []gitFact{ado("unknown")}, review: []gitFact{ado("unavailable")}},
		{name: "both", policy: open, repos: []string{scmTestADORepo, ghRepo},
			preview: []gitFact{ado("unknown"), github("direct")}, review: []gitFact{ado("needs_input"), github("direct")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := adoRunHarness(t, true)
			st.siteConfig = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
				{ID: scmTestRowID, Kind: types.GitProviderAzureDevOps, BaseURLs: []string{scmTestADOOrg},
					Lanes: []types.GitLane{types.GitLaneEntra}, CredentialSource: types.CredentialSourcePerUser,
					Entra: &types.ADOEntraConfig{TenantID: "tenant-1", ClientID: "client-1"}},
				{ID: "github-private-row", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com"}, Lanes: tc.lanes},
			}}}
			srv.cfg.DefaultPolicy = tc.policy
			if tc.connected {
				if err := srv.storeADOEntraBlob(context.Background(), "sub-ado-op", scmTestRowID, adoEntraBlob{
					RefreshToken: "rt", Scopes: scmTestBaseline(t), TenantID: "tenant-1", ClientID: "client-1",
					Subject: "sub-ado-op", Source: adoEntraSourceSignIn,
				}); err != nil {
					t.Fatal(err)
				}
			}
			var sources []types.WorkspaceSource
			for i, repo := range tc.repos {
				sources = append(sources, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: repo, Target: "/home/agent/r" + strconv.Itoa(i)})
			}
			body := `{"agent":"claude-code","task":"do the thing","workspace_id":"` + adoCreateWorkspace(t, st, sources...).String() + `"}`
			op := adoOperatorToken(st)
			if tc.mechanism {
				op = adminToken
			}
			for door, want := range map[string][]gitFact{componentDoors[2]: tc.preview, componentDoors[1]: tc.review} {
				w := do(t, srv, http.MethodPost, door, op, body)
				got := gitFacts(t, door, w)
				if len(got) != len(want) {
					t.Fatalf("%s git_provider facts = %+v, want %+v", door, got, want)
				}
				for i := range want {
					if got[i].Provider != want[i].Provider || got[i].ID != want[i].ID || got[i].Reason != want[i].Reason ||
						got[i].Status != want[i].Status || got[i].Lane != want[i].Lane || got[i].Org != want[i].Org || !slices.Equal(got[i].Repos, want[i].Repos) {
						t.Errorf("%s git_provider facts[%d] = %+v, want %+v", door, i, got[i], want[i])
					}
				}
				// The Git fact and Review's own git_credential fact agree: a
				// caller with nothing to sign in as is offered no sign-in.
				if tc.mechanism && door == componentDoors[1] && !strings.Contains(w.Body.String(), `"git_credential":{"state":"not_applicable"`) {
					t.Errorf("%s git_credential is not not_applicable: %s", door, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "github-private-row") || strings.Contains(w.Body.String(), scmTestRowID) {
					t.Errorf("%s names a provider row: %s", door, w.Body.String())
				}
			}
		})
	}
}
