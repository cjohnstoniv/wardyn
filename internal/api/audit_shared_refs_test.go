// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What an organisation's shared secret is called never reaches the person
// whose run uses it: not through the rows dispatch and the broker write for
// that run, not through a row written before the rule existed, and not
// through the run's grant list. The security tier reads every row as it was
// recorded, and a revive needs nothing from any of them.

// sharedRun is a member's run that carries an organisation's component with a
// shared secret, launched through the real create door and dispatched.
type sharedRun struct {
	f     *componentFixture
	id    uuid.UUID
	owner string
	grant types.CredentialGrant // the shared grant the create door persisted
}

func launchSharedRun(t *testing.T) *sharedRun {
	t.Helper()
	return launchSharedRunOn(t, newComponentFixture(t))
}

// newComponentViewFixture is newComponentFixture over a store that also serves
// a run's audit rows, which the run's policy view is built from.
func newComponentViewFixture(t *testing.T) *componentFixture {
	t.Helper()
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
	return &componentFixture{srv: New(cfg), st: st, cs: cs, rec: rec}
}

func launchSharedRunOn(t *testing.T, f *componentFixture) *sharedRun {
	t.Helper()
	ref := f.org(compOrgID, types.ComponentDefinition{
		Hosts: []string{"org-api.example"},
		Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}},
	})
	w := f.ask(t, componentDoors[0], componentBody(ref))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitForRecAudit(t, f.rec, created.ID, "run.policy.resolve", "success")
	run, err := f.st.GetRun(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := f.st.ListGrantsByRun(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(grants, func(g types.CredentialGrant) bool { return apiKeyScopeShared(g.Spec.Scope) })
	if i < 0 || !strings.Contains(string(grants[i].Spec.Scope), compOperatorSecret) {
		t.Fatalf("the create door persisted no shared grant naming the secret the sink reads: %+v", grants)
	}
	return &sharedRun{f: f, id: created.ID, owner: run.CreatedBy, grant: grants[i]}
}

// mintRows is the rows the broker writes when the proxy resolves grant, one
// per outcome: mintEvent's shape, which carries the grant's stored scope whole.
func mintRows(runID uuid.UUID, grant types.CredentialGrant) []types.AuditEvent {
	var rows []types.AuditEvent
	for _, outcome := range []string{"success", "denied", "failure"} {
		data := map[string]any{"grant_id": grant.ID.String(), "scope": grant.Spec.Scope}
		if outcome == "success" {
			data["jti"] = "jti-shared"
		}
		rows = append(rows, types.AuditEvent{ID: uuid.New(), RunID: &runID, ActorType: types.ActorAgent,
			Actor: "spiffe://wardyn.local/run/" + runID.String(), Action: "credential.mint",
			Target: grant.ID.String(), Outcome: outcome, Data: mustJSON(data)})
	}
	return rows
}

// trail is every row recorded for the run, then the broker's mint rows.
func (s *sharedRun) trail() []types.AuditEvent {
	var rows []types.AuditEvent
	for _, ev := range s.f.rec.snapshot() {
		if ev.RunID != nil && *ev.RunID == s.id {
			rows = append(rows, ev)
		}
	}
	return append(rows, mintRows(s.id, s.grant)...)
}

// sharedAuditStore is what the audit doors read of one run: who owns it, its
// rows through the paged reads, and its grant list.
type sharedAuditStore struct {
	auditScopeStore
	grants    []types.CredentialGrant
	grantsErr error
}

func (s *sharedAuditStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, s.grantsErr
}

// unpagedAuditStore is the same run behind a store with no pager, which the
// query answers through its fetch-all arm.
type unpagedAuditStore struct {
	store.Store
	run    types.AgentRun
	rows   []types.AuditEvent
	grants []types.CredentialGrant
}

func (s *unpagedAuditStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}

func (s *unpagedAuditStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return s.rows, nil
}

func (s *unpagedAuditStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}

// auditDoors is a server whose audit reads answer from st.
func auditDoors(t *testing.T, st store.Store) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg)
}

func sharedAuditDoors(t *testing.T, runID uuid.UUID, owner string, rows []types.AuditEvent, grants ...types.CredentialGrant) (*Server, *sharedAuditStore) {
	t.Helper()
	st := &sharedAuditStore{grants: grants}
	st.runs = map[uuid.UUID]types.AgentRun{runID: {ID: runID, CreatedBy: owner}}
	st.auditByRun = map[uuid.UUID][]types.AuditEvent{runID: rows}
	return auditDoors(t, st), st
}

func hasAction(rows []types.AuditEvent, action string) bool {
	return slices.ContainsFunc(rows, func(ev types.AuditEvent) bool { return ev.Action == action })
}

// The reported case end to end: an organisation's component with a shared
// secret, attached by a member, really dispatched; then the member's reads of
// that run's rows. The name is in none of the bytes served, and everything
// else about the grant — its host, its mark, its id — still is.
func TestAudit_AMemberNeverReadsASharedSecretsName(t *testing.T) {
	run := launchSharedRun(t)
	rows := run.trail()
	for _, action := range []string{"run.policy.resolve", "credential.mint"} {
		if !hasAction(rows, action) {
			t.Fatalf("the run's trail has no %s row; the reads below would prove nothing", action)
		}
	}
	paged, _ := sharedAuditDoors(t, run.id, run.owner, rows, run.grant)
	unpaged := auditDoors(t, &unpagedAuditStore{run: types.AgentRun{ID: run.id, CreatedBy: run.owner}, rows: rows,
		grants: []types.CredentialGrant{run.grant}})
	member := ssoSession(t, run.owner, capEmail, oidc.RoleUser)
	id := run.id.String()
	for name, read := range map[string]struct {
		srv  *Server
		path string
		want []string
	}{
		"the policy row":        {paged, "/api/v1/audit?run_id=" + id + "&action=run.policy.resolve", []string{"run.policy.resolve"}},
		"the mint rows":         {paged, "/api/v1/audit?run_id=" + id + "&action=credential.mint", []string{"credential.mint", run.grant.ID.String()}},
		"the run's whole trail": {paged, "/api/v1/audit?run_id=" + id, []string{"run.policy.resolve", "credential.mint"}},
		"the export":            {paged, "/api/v1/audit/export?run_id=" + id, []string{"run.policy.resolve", "credential.mint"}},
		"a store with no pager": {unpaged, "/api/v1/audit?run_id=" + id, []string{"run.policy.resolve", "credential.mint"}},
	} {
		t.Run(name, func(t *testing.T) {
			w := doSSO(t, read.srv, http.MethodGet, read.path, member, "")
			body := w.Body.String()
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d %s, want 200", read.path, w.Code, body)
			}
			if strings.Contains(body, compOperatorSecret) {
				t.Errorf("the run's owner reads the organisation's secret name: %s", body)
			}
			for _, want := range append(read.want, "org-api.example", `"shared":true`) {
				if !strings.Contains(body, want) {
					t.Errorf("the read lacks %q, so the rows were dropped rather than served without the name: %s", want, body)
				}
			}
		})
	}
}

// What dispatch records for the run is the policy with every grant scope
// whole, the organisation's secret name included: the security tier reads
// that row, and the sinks receive it. The grant row the sink and a revive read
// carries the same scope. The run's owner is served the recorded row without
// the name, and a person's own secret keeps its name for them.
func TestDispatch_TheRecordedPolicyKeepsEveryGrantScopeWhole(t *testing.T) {
	const orgScope = `{"host":"org-api.example","require_tls":true,"secret_name":"operator-only-secret","shared":true}`
	f := newComponentFixture(t)
	org := f.org(compOrgID, types.ComponentDefinition{
		Hosts: []string{"org-api.example"},
		Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}},
	})
	person := inlineComponent([]string{"person-api.example"}, headerSecret(compOwnSecret, "person-api.example"))
	w := f.ask(t, componentDoors[0], componentBody(person, org))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	ev := waitForRecAudit(t, f.rec, created.ID, "run.policy.resolve", "success")
	var spec types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &spec); err != nil {
		t.Fatal(err)
	}
	var scopes []string
	for _, g := range spec.EligibleGrants {
		scopes = append(scopes, string(g.Scope))
	}
	if want := []string{`{"host":"person-api.example","require_tls":true,"secret_name":"person-secret"}`, orgScope}; !slices.Equal(scopes, want) {
		t.Errorf("recorded grant scopes =\n %v\nwant\n %v", scopes, want)
	}
	grants, err := f.st.ListGrantsByRun(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(grants, func(g types.CredentialGrant) bool { return string(g.Spec.Scope) == orgScope }) {
		t.Errorf("the run's grant rows lost the scope the sink resolves: %+v", grants)
	}

	run, err := f.st.GetRun(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := sharedAuditDoors(t, created.ID, run.CreatedBy, []types.AuditEvent{*ev}, grants...)
	body := doSSO(t, srv, http.MethodGet, "/api/v1/audit?run_id="+created.ID.String(), ssoSession(t, run.CreatedBy, capEmail, oidc.RoleUser), "").Body.String()
	if strings.Contains(body, compOperatorSecret) || !strings.Contains(body, `"secret_name":"person-secret"`) ||
		!strings.Contains(body, `"scope":{"host":"org-api.example","require_tls":true,"shared":true}`) {
		t.Errorf("the run's owner is served the recorded policy as %s, want it without the organisation's secret name and with their own", body)
	}
}

// The run's policy view is the recorded policy under another door (and what
// `wardyn run policy` exports). An admin and a security admin read the shared
// grant with its secret name; the run's owner reads the same grant without it.
func TestRunPolicyView_TheSecurityTierReadsASharedGrantsSecretNameAndTheOwnerDoesNot(t *testing.T) {
	run := launchSharedRunOn(t, newComponentViewFixture(t))
	path := "/api/v1/runs/" + run.id.String() + "/policy"
	srv := run.f.srv
	for name, get := range map[string]func() *httptest.ResponseRecorder{
		"the admin token": func() *httptest.ResponseRecorder { return do(t, srv, http.MethodGet, path, adminToken, "") },
		"an admin": func() *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodGet, path, ssoSession(t, "root", "root@corp.example", oidc.RoleAdmin), "")
		},
		"a security admin": func() *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodGet, path, ssoSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin), "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := get()
			if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, `"scope":`+string(run.grant.Spec.Scope)) {
				t.Errorf("GET %s = %d %s, want the shared grant's scope whole, secret name included", path, w.Code, body)
			}
		})
	}
	w := doSSO(t, srv, http.MethodGet, path, ssoSession(t, run.owner, capEmail, oidc.RoleUser), "")
	if body := w.Body.String(); w.Code != http.StatusOK || strings.Contains(body, compOperatorSecret) ||
		!strings.Contains(body, `"scope":{"host":"org-api.example","require_tls":true,"shared":true}`) {
		t.Errorf("GET %s as the run's owner = %d %s, want the shared grant without the organisation's secret name", path, w.Code, body)
	}
}

// The security tier reads the rows as they were recorded: an admin's token, an
// admin's session and a security admin's all get the broker's mint rows with
// the scope whole, byte for byte.
func TestAudit_TheSecurityTierReadsTheRowsAsRecorded(t *testing.T) {
	run := launchSharedRun(t)
	rows := run.trail()
	srv, _ := sharedAuditDoors(t, run.id, run.owner, rows, run.grant)
	path := "/api/v1/audit?run_id=" + run.id.String()
	for name, get := range map[string]func() *httptest.ResponseRecorder{
		"the admin token": func() *httptest.ResponseRecorder { return do(t, srv, http.MethodGet, path, adminToken, "") },
		"an admin": func() *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodGet, path, ssoSession(t, "root", "root@corp.example", oidc.RoleAdmin), "")
		},
		"a security admin": func() *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodGet, path, ssoSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin), "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := get()
			var served []types.AuditEvent
			if err := json.Unmarshal(w.Body.Bytes(), &served); err != nil || w.Code != http.StatusOK || len(served) != len(rows) {
				t.Fatalf("GET %s = %d, %d rows (%v), want the run's %d", path, w.Code, len(served), err, len(rows))
			}
			for i := range rows {
				if string(served[i].Data) != string(rows[i].Data) {
					t.Errorf("%s row served as\n %s\nrecorded as\n %s", rows[i].Action, served[i].Data, rows[i].Data)
				}
			}
			if n := strings.Count(w.Body.String(), `"scope":`+string(run.grant.Spec.Scope)); n != 4 {
				t.Errorf("the grant's scope is served whole on %d rows, want the recorded policy and the three mint rows", n)
			}
		})
	}
}

// Rows written before the rule are served under it. The recorded policy,
// under its current action name and its pre-0.8 one, and a
// dropped injection, which names its grant by id and carries no mark of its
// own: the organisation's name is gone from each, a person's own secret keeps
// its name, and a row with nothing to remove is served as it was written.
func TestAudit_ARowWrittenBeforeTheRuleIsServedWithoutTheName(t *testing.T) {
	const own = "person-secret"
	runID, owner := uuid.New(), "sub-earlier-run"
	shared := types.CredentialGrant{ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{Kind: types.GrantAPIKey,
		Scope: json.RawMessage(sharedScope)}}
	theirs := types.CredentialGrant{ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, OwnerOnly: true,
		Scope: json.RawMessage(`{"host":"api.openai.com","require_tls":true,"secret_name":"` + own + `"}`)}}
	policy := `{"allowed_domains":["org-api.example"],"resources":{"disk_mib":9007199254740993},"eligible_grants":[` +
		`{"kind":"api_key","scope":` + sharedScope + `,"ttl_seconds":3600},` +
		`{"kind":"env_secret","owner_only":true,"scope":{"name":"PERSON_TOKEN","secret_name":"` + own + `"}}]}`
	row := func(action, data string) types.AuditEvent {
		return types.AuditEvent{ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Actor: "wardynd",
			Action: action, Outcome: "success", Data: json.RawMessage(data)}
	}
	drop := func(g types.CredentialGrant, name string) string {
		return `{"grant_id":"` + g.ID.String() + `","host":"api.openai.com","reason":"model_credential_not_provider_authored","secret_name":"` + name + `"}`
	}
	rows := []types.AuditEvent{
		row("run.policy.resolve", policy),
		row("run.policy.effective", policy),
		row("run.injection.drop", drop(shared, sharedSecretName)),
		row("run.injection.drop", drop(theirs, own)),
		row("run.exec", `{"z":1,"a":[2,{"b":null}]}`),
	}
	srv, st := sharedAuditDoors(t, runID, owner, rows, shared, theirs)
	member := ssoSession(t, owner, "earlier@corp.example", oidc.RoleUser)
	for _, path := range []string{"/api/v1/audit?run_id=" + runID.String(), "/api/v1/audit/export?run_id=" + runID.String()} {
		w := doSSO(t, srv, http.MethodGet, path, member, "")
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s, want 200", path, w.Code, body)
		}
		if strings.Contains(body, sharedSecretName) {
			t.Errorf("GET %s serves the organisation's secret name from an earlier row: %s", path, body)
		}
		for what, want := range map[string]string{
			"the shared grant's host and mark":  `"scope":{"host":"org-api.example","require_tls":true,"shared":true}`,
			"a person's own env secret":         `"scope":{"name":"PERSON_TOKEN","secret_name":"person-secret"}`,
			"a number wider than a float":       `"disk_mib":9007199254740993`,
			"the shared grant's dropped row":    `{"grant_id":"` + shared.ID.String() + `","host":"api.openai.com","reason":"model_credential_not_provider_authored"}`,
			"a person's own dropped row":        drop(theirs, own),
			"a row with nothing to remove":      `{"z":1,"a":[2,{"b":null}]}`,
			"the grants beside the shared one":  `"ttl_seconds":3600`,
			"the policy's allowlist":            `"allowed_domains":["org-api.example"]`,
			"the env grant's owner_only stamp":  `"owner_only":true`,
			"the dropped row's own reason code": `model_credential_not_provider_authored`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s lost %s (%s): %s", path, what, want, body)
			}
		}
	}

	// A row that names a grant beside a secret cannot be served until the
	// run's grant list says whether that grant is shared.
	st.grantsErr = errors.New("connection refused")
	for path, want := range map[string]int{
		"/api/v1/audit?run_id=" + runID.String():        http.StatusInternalServerError,
		"/api/v1/audit/export?run_id=" + runID.String(): http.StatusServiceUnavailable,
	} {
		w := doSSO(t, srv, http.MethodGet, path, member, "")
		if body := w.Body.String(); w.Code != want || strings.Contains(body, sharedSecretName) || strings.Contains(body, "connection refused") {
			t.Errorf("GET %s with the grant list unreadable = %d %s, want %d and neither the name nor the store's error", path, w.Code, body, want)
		}
	}
}

// The strip reads the mark and nothing else about a value's shape.
func TestWithoutSharedSecretRefs(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"a shared scope":          {sharedScope, `{"host":"org-api.example","require_tls":true,"shared":true}`},
		"every secret-naming key": {`{"shared":true,"key_secret_ref":"k","known_hosts_secret_ref":"h","secret_name":"n","host":"x"}`, `{"host":"x","shared":true}`},
		"nested in a list":        {`[{"a":{"scope":` + sharedScope + `}}]`, `[{"a":{"scope":{"host":"org-api.example","require_tls":true,"shared":true}}}]`},
		"shared false":            {`{"secret_name":"n","shared":false}`, `{"secret_name":"n","shared":false}`},
		"shared not a boolean":    {`{"secret_name":"n","shared":"true"}`, `{"secret_name":"n","shared":"true"}`},
		"no mark":                 {`{"secret_name":"n", "host":"x"}`, `{"secret_name":"n", "host":"x"}`},
		"a mark and no name":      {`{"shared":true,  "host":"x"}`, `{"shared":true,  "host":"x"}`},
		"not JSON":                {`{"secret_name":"n","shared":true`, ``},
		"empty":                   {``, ``},
	} {
		t.Run(name, func(t *testing.T) {
			if got := string(withoutSharedSecretRefs(json.RawMessage(tc.in))); got != tc.want {
				t.Errorf("withoutSharedSecretRefs(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// A revive that strips a shared grant's rule, because its host serves a model
// now, records the drop with the rule's secret name as it does for every
// stripped rule. The row names its grant by id and carries no mark, so the
// run's grant list is what tells the read that the name is the organisation's.
func TestAudit_ADroppedSharedInjectionIsServedWithoutTheName(t *testing.T) {
	const uid = "0b6f2c9e-5d7a-4c1b-9a3e-2f8d6b4a1c70"
	f, _ := newOwnerFixture(t)
	p := keyProvider("anthropic", "claude-code")
	p.UID = uid
	f.st.run.ModelProviderID = "anthropic"
	f.st.site.ModelProviders = &types.ModelProviders{Providers: []types.ModelProvider{p}}
	f.st.credGrants = []types.CredentialGrant{{ID: uuid.New(), RunID: f.run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey,
		Scope: mustJSON(map[string]any{"host": "api.anthropic.com", "secret_name": sharedSecretName, "shared": true})}}}
	f.editConfig(t, func(c *proxy.Config) {
		c.Injection = append(c.Injection, proxy.InjectionConfig{GrantID: f.st.credGrants[0].ID,
			InjectionRule: egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", Format: "%s", SecretName: sharedSecretName}})
	})
	sec := &memSecrets{m: map[string][]byte{sharedSecretName: []byte(sharedOperator)}}
	if err := sec.For(f.run.CreatedBy).Put(context.Background(), providerSecretName(uid, providerKeyPart), []byte("sk-ant-own")); err != nil {
		t.Fatal(err)
	}
	f.srv.cfg.Secrets = sec
	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	drops := f.audit.eventsFor(f.run.ID, "run.injection.drop")
	if len(drops) != 1 || !strings.Contains(string(drops[0].Data), `"secret_name":"`+sharedSecretName+`"`) {
		t.Fatalf("run.injection.drop rows = %+v, want one for the shared grant, recorded as every drop is", drops)
	}
	srv, _ := sharedAuditDoors(t, f.run.ID, f.run.CreatedBy, drops, f.st.credGrants...)
	member := ssoSession(t, f.run.CreatedBy, ownerEmail, oidc.RoleUser)
	for _, path := range []string{"/api/v1/audit?run_id=" + f.run.ID.String(), "/api/v1/audit/export?run_id=" + f.run.ID.String()} {
		w := doSSO(t, srv, http.MethodGet, path, member, "")
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, "run.injection.drop") || !strings.Contains(body, f.st.credGrants[0].ID.String()) ||
			!strings.Contains(body, "api.anthropic.com") {
			t.Fatalf("GET %s = %d %s, want the drop row", path, w.Code, body)
		}
		if strings.Contains(body, sharedSecretName) {
			t.Errorf("GET %s serves the organisation's secret name on the drop row: %s", path, body)
		}
	}
}

// noAuditReads fails a test in which a revive reads the run's audit rows.
type noAuditReads struct {
	*reviveStore
	t *testing.T
}

func (s *noAuditReads) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	s.t.Error("the revive read the run's audit rows")
	return nil, nil
}

// A revive takes a shared credential from the run's grant rows and its stored
// proxy config, and reads no audit row to do it: what a run's audit rows say,
// or are served as, changes nothing about what the revived proxy injects.
func TestRevive_ASharedCredentialNeedsNothingFromTheRunsAuditRows(t *testing.T) {
	f, _ := newModelCredFixture(t)
	scope := mustJSON(map[string]any{"host": "artifactory.corp.example", "secret_name": "artifactory-token", "shared": true})
	f.st.credGrants[0].Spec.Scope = scope
	f.st.site.Integrations = nil
	f.srv.cfg.Store = &noAuditReads{reviveStore: f.rs, t: t}

	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	cfg := f.newConfig(t)
	i := slices.IndexFunc(cfg.Injection, func(in proxy.InjectionConfig) bool { return in.Host == "artifactory.corp.example" })
	if i < 0 || cfg.Injection[i].GrantID != f.st.credGrants[0].ID || cfg.Injection[i].Header != "x-api-key" {
		t.Errorf("revived injections = %+v, want the shared grant's rule as it was stored", cfg.Injection)
	}
	if string(f.st.credGrants[0].Spec.Scope) != string(scope) {
		t.Errorf("the grant row's scope changed: %s", f.st.credGrants[0].Spec.Scope)
	}
}
