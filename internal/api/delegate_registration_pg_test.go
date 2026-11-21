// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The registration tests below run on Postgres (WARDYN_TEST_PG) over the delegation harness: a real store,
// a real registry lock and a real census.

type regPG struct {
	*delegationPG
	admin2 *http.Cookie
}

// newRegPG is a harness with issuer portals switched on and two super admins.
func newRegPG(t *testing.T) *regPG {
	t.Helper()
	e := &regPG{delegationPG: newDelegationPG(t)}
	if activated, err := e.st.ActivatePortalIssuers(context.Background(), "test", nil); err != nil || !activated {
		t.Fatalf("ActivatePortalIssuers = %v, %v, want the first activation to report true", activated, err)
	}
	e.admin2 = accessSession(t, "root2", "root2@corp.example", oidc.RoleAdmin, []string{})
	return e
}

// body is a valid access-token registration pinned to the Default unit; mut edits it (nil deletes a key).
func (e *regPG) body(mut map[string]any) string {
	m := map[string]any{"run_in_under": store.DefaultUnitID.String()}
	for k, v := range mut {
		m[k] = v
	}
	return regBody(m)
}

func (e *regPG) register(t *testing.T, mut map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", e.admin, e.body(mut))
}

func (e *regPG) registered(t *testing.T, mut map[string]any) types.Delegate {
	t.Helper()
	w := e.register(t, mut)
	if w.Code != http.StatusCreated {
		t.Fatalf("register %v = %d %s, want 201", mut, w.Code, w.Body.String())
	}
	var d types.Delegate
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *regPG) refused(t *testing.T, name string, mut map[string]any, status int, reason string) *httptest.ResponseRecorder {
	t.Helper()
	w := e.register(t, mut)
	if w.Code != status || reasonOf(t, w) != reason {
		t.Errorf("%s: %d %s, want %d %s", name, w.Code, w.Body.String(), status, reason)
	}
	return w
}

// portals is how many portal rows an issuer has.
func (e *regPG) portals(t *testing.T, issuer string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM delegates WHERE issuer = $1`, issuer).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// orphan seeds one historical record that names a person under issuer's key, with no binding: a unit
// created by such a principal.
func (e *regPG) orphan(t *testing.T, issuer string) string {
	t.Helper()
	p := types.PortalPrincipal(types.PortalIssuerKey(issuer), "orphan")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}',$3)`,
		uuid.New(), "orphan-"+uuid.NewString(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A historical run owned by a portal person of a key no binding holds, with no identity row, makes the
// registration of that issuer fail delegate_namespace_in_use.
func TestPortalRegistration_NamespaceInUseRefused_PG(t *testing.T) {
	e := newRegPG(t)
	const issuer = "https://orphaned.example.com"
	owner := types.PortalPrincipal(types.PortalIssuerKey(issuer), "old")
	if _, err := e.st.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunCompleted, RunnerTarget: "docker", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	w := e.refused(t, "orphaned run", map[string]any{"issuer": issuer}, http.StatusConflict, reasonDelegateNamespaceInUse)
	if !strings.Contains(w.Body.String(), "agent_runs.created_by (1)") || strings.Contains(w.Body.String(), owner) {
		t.Errorf("the refusal must name the column and count and never the value: %s", w.Body.String())
	}
	if e.portals(t, issuer) != 0 {
		t.Error("a refused registration stored a portal")
	}
	var bindings int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM portal_issuers WHERE issuer = $1`, issuer).Scan(&bindings)
	if bindings != 0 {
		t.Error("a refused registration left a binding")
	}
	e.registered(t, map[string]any{"issuer": "https://clean.example.com"})
}

// Each independent historical source the store census reports refuses a registration on its own, and a
// key whose only history is an ended run's policy rows registers. The records are written by the store's own
// writers where it has them, the way the census reads them: a unit's creator, a running run's owner, a running
// run's resolved policy and its policy generation, a launch preset, a unit ceiling, a composed unit's overlay,
// a run template and a queued launch.
func TestPortalRegistration_NamespaceCensusRefusesEachSource_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	newRun := func(owner string, state types.RunState) uuid.UUID {
		r, err := e.st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
			ConfinementClass: types.CC3, State: state, RunnerTarget: "docker", Task: "t"})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	policyRow := func(run uuid.UUID, action, principal string) {
		data := `{"ui_apps":[{"name":"a","callers":["user:` + principal + `"]}]}`
		if action == "run.policy.generation" {
			data = `{"generation":2,"policy":` + data + `}`
		}
		if err := store.InsertAuditEvent(ctx, e.pool, &types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &run,
			ActorType: types.ActorSystem, Actor: "test", Action: action, Outcome: "success", Data: json.RawMessage(data)}); err != nil {
			t.Fatal(err)
		}
	}
	// whileBound writes a stored document naming principal while issuer's key is bound, as every ruled writer
	// needs, then drops the binding, so the key is new again to the registration.
	whileBound := func(issuer string, write func(principal string) error) {
		key := types.PortalIssuerKey(issuer)
		if _, err := e.st.BindPortalIssuerQ(ctx, e.pool, issuer, "", "uid", "test"); err != nil {
			t.Fatal(err)
		}
		if err := write(types.PortalPrincipal(key, "p")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `DELETE FROM portal_issuers WHERE issuer_key = $1`, key); err != nil {
			t.Fatal(err)
		}
	}
	uiApps := func(p string) []types.UIApp {
		return []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{"user:" + p}}}
	}
	uiAppsDoc := func(p string) string {
		return `{"ui_apps":[{"name":"app","port":8080,"inbound":true,"callers":["user:` + p + `"]}]}`
	}
	writeUnit := func(u types.GovernanceUnit) error {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		u.Name, u.CreatedBy = "unit-"+uuid.NewString(), "admin@example.com"
		if _, err := store.WriteGovernanceUnitQ(ctx, tx, uuid.New(), func([]types.GovernanceUnit, []types.GovernanceMembership) (types.GovernanceUnit, error) {
			return u, nil
		}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	parent := store.DefaultUnitID
	sources := []struct {
		name, column string
		seed         func(issuer string)
	}{
		{"a unit's creator", "governance_units.created_by", func(issuer string) {
			p := types.PortalPrincipal(types.PortalIssuerKey(issuer), "p")
			if _, err := e.pool.Exec(ctx, `INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}',$3)`, uuid.New(), "u-"+uuid.NewString(), p); err != nil {
				t.Fatal(err)
			}
		}},
		{"a run's owner", "agent_runs.created_by", func(issuer string) {
			newRun(types.PortalPrincipal(types.PortalIssuerKey(issuer), "p"), types.RunRunning)
		}},
		{"a running run's resolved policy", "audit_events.data", func(issuer string) {
			policyRow(newRun("sub-owner", types.RunRunning), "run.policy.resolve", types.PortalPrincipal(types.PortalIssuerKey(issuer), "p"))
		}},
		{"a running run's policy generation", "audit_events.data", func(issuer string) {
			policyRow(newRun("sub-owner", types.RunRunning), "run.policy.generation", types.PortalPrincipal(types.PortalIssuerKey(issuer), "p"))
		}},
		{"a launch preset", "launch_presets.request", func(issuer string) {
			whileBound(issuer, func(p string) error {
				_, _, err := e.st.PutLaunchPreset(ctx, types.LaunchPreset{Name: "preset-" + uuid.NewString(), UserTypes: []string{},
					Request: json.RawMessage(`{"task":"x","inline_policy":` + uiAppsDoc(p) + `}`), CreatedBy: "admin@example.com"})
				return err
			})
		}},
		{"a unit ceiling", "governance_units.ceiling", func(issuer string) {
			whileBound(issuer, func(p string) error {
				return writeUnit(types.GovernanceUnit{Ceiling: types.RunPolicySpec{UIApps: uiApps(p)}})
			})
		}},
		{"a composed unit's overlay", "governance_units.overlay", func(issuer string) {
			whileBound(issuer, func(p string) error {
				apps := uiApps(p)
				return writeUnit(types.GovernanceUnit{ParentID: &parent, Overlay: &types.CeilingOverlay{UIApps: &apps}})
			})
		}},
		{"a run template", "run_template_revisions.document", func(issuer string) {
			whileBound(issuer, func(p string) error {
				_, err := e.st.CreateTemplate(ctx, types.RunTemplate{ID: uuid.New(), Owner: types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"},
					Kind: types.TemplateKindTemplate, Name: "t-" + uuid.NewString(), CreatedBy: "alice",
					Document: json.RawMessage(`{"kind":"template","intent":{"inline_policy":` + uiAppsDoc(p) + `}}`)}, nil)
				return err
			})
		}},
		{"a queued launch", "run_queue.launch", func(issuer string) {
			pool := uuid.New()
			if _, err := e.pool.Exec(ctx, `INSERT INTO runner_pools (id, name, scope, owner) VALUES ($1, $2, 'user', 'sub-census')`, pool, "pool-"+uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			run := newRun("sub-owner", types.RunQueued)
			doc := `{"task":"x","inline_policy":` + uiAppsDoc(types.PortalPrincipal(types.PortalIssuerKey(issuer), "p")) + `}`
			if _, err := e.pool.Exec(ctx, `INSERT INTO run_queue (run_id, pool_id, owner, deadline_at, launch) VALUES ($1, $2, 'sub-owner', now() + interval '1 hour', $3::jsonb)`,
				run, pool, doc); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for i, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			issuer := "https://source-" + strings.Repeat("x", i+1) + ".example.com"
			src.seed(issuer)
			w := e.refused(t, src.name, map[string]any{"issuer": issuer}, http.StatusConflict, reasonDelegateNamespaceInUse)
			if !strings.Contains(w.Body.String(), src.column+" (1)") {
				t.Errorf("the refusal does not name %s once: %s", src.column, w.Body.String())
			}
			if e.portals(t, issuer) != 0 {
				t.Errorf("a refused registration stored a portal")
			}
			// Another issuer is not touched by it.
			e.registered(t, map[string]any{"issuer": "https://untouched-" + strings.Repeat("y", i+1) + ".example.com"})
		})
	}
	t.Run("only an ended run's policy rows", func(t *testing.T) {
		const issuer = "https://ended-only.example.com"
		p := types.PortalPrincipal(types.PortalIssuerKey(issuer), "p")
		ended := newRun("sub-owner", types.RunCompleted)
		policyRow(ended, "run.policy.resolve", p)
		policyRow(ended, "run.policy.generation", p)
		policyRow(newRun("sub-owner", types.RunRunning), "run.started", p) // not a policy row
		if d := e.registered(t, map[string]any{"issuer": issuer}); d.IssuerKey != types.PortalIssuerKey(issuer) {
			t.Errorf("issuer key = %q", d.IssuerKey)
		}
	})
}

// Several portals on one issuer coexist: another pin and another credential are other portals on one binding,
// while another address, another person-id claim and another spelling of a bound issuer are refused.
func TestPortalRegistration_SeveralIssuerPortalsCoexist_PG(t *testing.T) {
	e := newRegPG(t)
	t.Setenv(envPortalInternalIssuerHosts, "idp.internal.example.com")
	const issuer = "https://shared.example.com/oauth2/default"
	unit2 := uuid.New()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,'second','{}','{}','t')`, unit2); err != nil {
		t.Fatal(err)
	}
	a := e.registered(t, map[string]any{"issuer": issuer, "name": "a"})
	b := e.registered(t, map[string]any{"issuer": issuer, "name": "b", "run_in_under": unit2.String()})
	other := e.registered(t, map[string]any{"issuer": "https://other.example.com", "name": "c"})
	if a.IssuerKey == "" || a.IssuerKey != b.IssuerKey || a.IssuerKey == other.IssuerKey || a.Credential == b.Credential {
		t.Errorf("keys %q %q %q: one issuer is one key, another issuer another", a.IssuerKey, b.IssuerKey, other.IssuerKey)
	}
	if !strings.HasPrefix(a.Credential, store.PortalCredentialPrefix) {
		t.Errorf("credential %q lacks the issuer portal prefix", a.Credential)
	}
	var bindings int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM portal_issuers WHERE issuer = $1`, issuer).Scan(&bindings)
	if bindings != 1 || e.portals(t, issuer) != 2 {
		t.Errorf("bindings %d portals %d, want 1 and 2", bindings, e.portals(t, issuer))
	}
	e.refused(t, "another address", map[string]any{"issuer": issuer, "internal_issuer": "https://idp.internal.example.com"}, 409, reasonDelegateIssuerAddressConflict)
	e.refused(t, "another person-id claim", map[string]any{"issuer": issuer, "token_profile": "okta_id_token", "audience": nil, "required_scopes": nil}, 409, reasonDelegateIssuerProfileConflict)
	e.refused(t, "another spelling", map[string]any{"issuer": issuer + "/"}, 409, reasonDelegateIssuerSpellingConflict)
	e.refused(t, "another case of the host", map[string]any{"issuer": "https://SHARED.example.com/oauth2/default"}, 409, reasonDelegateIssuerSpellingConflict)
}

// A portal in real use does not stop a second portal on its issuer or a rotation (register the new
// credential, revoke the old), while the key of an unrelated historical record stays refused.
func TestPortalRegistration_SecondPortalAndRotationAfterRealUse_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	const issuer = "https://used.example.com"
	a := e.registered(t, map[string]any{"issuer": issuer, "name": "a"})
	person := types.PortalPrincipal(a.IssuerKey, "00u1")
	if _, _, err := e.st.IssuePortalIdentity(ctx, person, a.IssuerKey, "p@corp.example", issuer, a.ID, a.AdmissionRev); err != nil {
		t.Fatalf("introduce a person: %v", err)
	}
	if _, err := e.st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: person, Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunCompleted, RunnerTarget: "docker", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser, Subject: person,
		Capability: "allowed_domains", Value: "pypi.org", Effect: types.CapabilityAllow, CreatedBy: "test"}); err != nil {
		t.Fatalf("grant to the person: %v", err)
	}
	orphanIssuer := "https://unrelated-orphan.example.com"
	e.orphan(t, orphanIssuer)

	second := e.registered(t, map[string]any{"issuer": issuer, "name": "second"})
	rotated := e.registered(t, map[string]any{"issuer": issuer, "name": "rotated"})
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+a.ID.String(), e.secAdmin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke the rotated-out portal = %d %s", w.Code, w.Body.String())
	}
	if second.IssuerKey != a.IssuerKey || rotated.IssuerKey != a.IssuerKey {
		t.Error("a second portal or a rotation changed the issuer's key")
	}
	e.refused(t, "the orphan's key", map[string]any{"issuer": orphanIssuer}, 409, reasonDelegateNamespaceInUse)
}

// Two registrations that conflict, started together, are serialised by the registry lock: exactly one wins
// and the other gets its 409. That holds for two direct registrations and for a held approval against a direct
// registration, whichever reaches the lock first.
func TestPortalRegistry_ConcurrentRegistrationsSerialised_PG(t *testing.T) {
	e := newRegPG(t)
	t.Setenv(envPortalInternalIssuerHosts, "a.internal.example.com,b.internal.example.com")
	race := func(name string, one, two map[string]any, loser string) {
		t.Helper()
		var wg sync.WaitGroup
		out := make([]*httptest.ResponseRecorder, 2)
		start := make(chan struct{})
		for i, mut := range []map[string]any{one, two} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out[i] = e.register(t, mut)
			}()
		}
		close(start)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the two registrations did not finish within 30 s", name)
		}
		created, refused := 0, 0
		for _, w := range out {
			switch {
			case w.Code == http.StatusCreated:
				created++
			case w.Code == http.StatusConflict && reasonOf(t, w) == loser:
				refused++
			default:
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		}
		if created != 1 || refused != 1 {
			t.Errorf("%s: %d created, %d refused with %s; want one each", name, created, refused, loser)
		}
	}
	race("two addresses", map[string]any{"issuer": "https://race-a.example.com", "internal_issuer": "https://a.internal.example.com"},
		map[string]any{"issuer": "https://race-a.example.com", "internal_issuer": "https://b.internal.example.com"}, reasonDelegateIssuerAddressConflict)
	race("two spellings", map[string]any{"issuer": "https://race-b.example.com/x"}, map[string]any{"issuer": "https://race-b.example.com/x/"}, reasonDelegateIssuerSpellingConflict)

	// A held approval against a conflicting direct registration of the same issuer at another internal address,
	// in both orders. A transaction of the test holds the registry lock until both requests are queued behind
	// it, as the database reports, so the one queued first takes the lock first.
	observer := e.sidePool(t)
	ctx := context.Background()
	heldRace := func(name, issuer string, approvalFirst bool) {
		t.Setenv(envGovernanceSecondHuman, "true")
		change, _, w := e.proposeHeld(t, map[string]any{"issuer": issuer, "internal_issuer": "https://a.internal.example.com"})
		if change == "" {
			t.Fatalf("%s: proposal = %d %s, want 202", name, w.Code, w.Body.String())
		}
		t.Setenv(envGovernanceSecondHuman, "") // the direct registration is not held
		gate, err := e.sidePool(t).Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Registered after the gate's pool, so it runs before that pool and the server's close: on every exit
		// the gate is released, which lets both requests finish, and both are joined.
		var workers sync.WaitGroup
		t.Cleanup(func() { rollbackWithin(gate); joinWithin(t, &workers, name+": the two requests") })
		if err := store.LockPortalIssuerRegistry(ctx, gate); err != nil {
			t.Fatal(err)
		}
		gatePID := backendPID(t, gate)
		approved, direct := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
		run := func(f func()) { workers.Add(1); go func() { defer workers.Done(); f() }() }
		start := []func(){
			func() { run(func() { approved <- e.decideHeld(t, change, "approve") }) },
			func() {
				run(func() {
					direct <- e.register(t, map[string]any{"issuer": issuer, "internal_issuer": "https://b.internal.example.com"})
				})
			},
		}
		if !approvalFirst {
			start[0], start[1] = start[1], start[0]
		}
		start[0]()
		first := waitBlockedBy(t, observer, gatePID)
		start[1]()
		waitBlockedBy(t, observer, gatePID, first)
		if err := gate.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wa, wd := receiveWithin(t, approved, name+": the approval"), receiveWithin(t, direct, name+": the direct registration")
		winner, loser, wantNorm := wa, wd, "https://a.internal.example.com"
		if !approvalFirst {
			winner, loser, wantNorm = wd, wa, "https://b.internal.example.com"
		}
		if winner.Code != http.StatusOK && winner.Code != http.StatusCreated {
			t.Errorf("%s: first in = %d %s, want success", name, winner.Code, winner.Body.String())
		}
		if loser.Code != http.StatusConflict || reasonOf(t, loser) != reasonDelegateIssuerAddressConflict {
			t.Errorf("%s: second in = %d %s, want 409 %s", name, loser.Code, loser.Body.String(), reasonDelegateIssuerAddressConflict)
		}
		var bindings int
		var norm string
		if err := e.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(internal_issuer_norm), '') FROM portal_issuers WHERE issuer_key = $1`,
			types.PortalIssuerKey(issuer)).Scan(&bindings, &norm); err != nil {
			t.Fatal(err)
		}
		if bindings != 1 || norm != types.NormIssuerURL(wantNorm) || e.portals(t, issuer) != 1 {
			t.Errorf("%s: %d binding(s) at %q and %d portal(s), want one of each at the first one's address", name, bindings, norm, e.portals(t, issuer))
		}
		wantState := types.GovernanceChangeApplied
		if !approvalFirst {
			wantState = types.GovernanceChangePending
		}
		if s := e.changeState(t, change); s != wantState {
			t.Errorf("%s: change state = %s, want %s", name, s, wantState)
		}
	}
	heldRace("approval first", "https://race-c.example.com", true)
	heldRace("direct first", "https://race-d.example.com", false)
}

// An issuer this deployment once served as its console issuer is refused, in any spelling equal under norm,
// even when no replica serves it now; another issuer is not.
func TestPortalRegistration_PastConsoleIssuerRefused_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RecordConsoleIssuerQ(ctx, tx, "https://old-console.example.com/tenant", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, iss := range []string{"https://old-console.example.com/tenant", "https://OLD-console.example.com:443/tenant/"} {
		e.refused(t, iss, map[string]any{"issuer": iss}, 409, reasonDelegateIssuerIsConsole)
	}
	e.registered(t, map[string]any{"issuer": "https://new-idp.example.com/tenant"})
}

// A trailing slash is another issuer and so another person: the second spelling is refused while the first
// has a binding, and the two spellings hash to different keys and principals.
func TestPortalIssuer_TrailingSlashSpellingIsDifferentPerson_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	const a = "https://slash.example.com/oauth2/a"
	d := e.registered(t, map[string]any{"issuer": a})
	if _, _, err := e.st.IssuePortalIdentity(ctx, types.PortalPrincipal(d.IssuerKey, "00u1"), d.IssuerKey, "", a, d.ID, d.AdmissionRev); err != nil {
		t.Fatal(err)
	}
	e.refused(t, "second spelling", map[string]any{"issuer": a + "/"}, 409, reasonDelegateIssuerSpellingConflict)
	if types.PortalIssuerKey(a) == types.PortalIssuerKey(a+"/") || types.PortalPrincipal(types.PortalIssuerKey(a), "00u1") == types.PortalPrincipal(types.PortalIssuerKey(a+"/"), "00u1") {
		t.Error("two spellings of one issuer gave one person")
	}
	if e.portals(t, a+"/") != 0 {
		t.Error("the refused spelling stored a portal")
	}
}

// The People list shows a portal person's issuer and internal address, the role user with no role-mapping
// read, and the display email; a person whose binding has no internal address shows none.
func TestPeopleList_PortalPersonShowsIssuerAndUser_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	t.Setenv(envPortalInternalIssuerHosts, "idp.internal.example.com")
	const withInternal, plain = "https://people-a.example.com/oauth2/default", "https://people-b.example.com"
	a := e.registered(t, map[string]any{"issuer": withInternal, "internal_issuer": "https://IDP.internal.example.com:443/oauth2/default/"})
	b := e.registered(t, map[string]any{"issuer": plain})
	pa, pb := types.PortalPrincipal(a.IssuerKey, "00uA"), types.PortalPrincipal(b.IssuerKey, "00uB")
	for _, c := range []struct {
		principal, key, email, issuer string
		d                             types.Delegate
	}{{pa, a.IssuerKey, "sam@corp.example", withInternal, a}, {pb, b.IssuerKey, "jo@corp.example", plain, b}} {
		if _, _, err := e.st.IssuePortalIdentity(ctx, c.principal, c.key, c.email, c.issuer, c.d.ID, c.d.AdmissionRev); err != nil {
			t.Fatal(err)
		}
	}
	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/people?limit=200", e.secAdmin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list people = %d %s", w.Code, w.Body.String())
	}
	var page struct {
		People []map[string]any `json:"people"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	byPrincipal := map[string]map[string]any{}
	for _, p := range page.People {
		byPrincipal[p["principal"].(string)] = p
	}
	sam, jo := byPrincipal[pa], byPrincipal[pb]
	if sam == nil || jo == nil {
		t.Fatalf("portal people missing from %s", w.Body.String())
	}
	if sam["issuer"] != withInternal || sam["internal_address"] != "https://idp.internal.example.com/oauth2/default" || sam["role"] != "user" || sam["email"] != "sam@corp.example" {
		t.Errorf("person with an internal address = %v", sam)
	}
	if jo["issuer"] != plain || jo["role"] != "user" || jo["email"] != "jo@corp.example" {
		t.Errorf("person without one = %v", jo)
	}
	if _, has := jo["internal_address"]; has {
		t.Errorf("a binding with no internal address shows one: %v", jo)
	}
}

// A held registration never makes its credential usable before a distinct super admin approves, whichever way
// the decision goes. Each failed decision leaves the change in its expected state, no portal row and no
// binding; the credential lookup answers not found (not a storage failure) and the exchange answers 401.
func TestPortalRegistration_HeldDecisions_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	t.Setenv(envGovernanceSecondHuman, "true")
	t.Setenv(envPortalInternalIssuerHosts, "idp.internal.example.com")
	type held struct {
		change, cred, issuer string
		id                   uuid.UUID
	}
	n := 0
	propose := func(t *testing.T, mut map[string]any) held {
		t.Helper()
		n++
		m := map[string]any{"issuer": "https://held-" + strings.Repeat("h", n) + ".example.com"}
		for k, v := range mut {
			m[k] = v
		}
		change, cred, w := e.proposeHeld(t, m)
		if change == "" || !strings.HasPrefix(cred, store.PortalCredentialPrefix) {
			t.Fatalf("propose = %d %s, want 202 with the credential once", w.Code, w.Body.String())
		}
		var key string
		if err := e.pool.QueryRow(ctx, `SELECT target_key FROM governance_changes WHERE id = $1`, change).Scan(&key); err != nil {
			t.Fatal(err)
		}
		return held{change: change, cred: cred, issuer: m["issuer"].(string), id: uuid.MustParse(key)}
	}
	decide := func(who *http.Cookie, change, verb string) *httptest.ResponseRecorder {
		return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/governance/changes/"+change+"/"+verb, who, `{"reason":"no"}`)
	}
	// unusable checks every durable trace of a registration that did not apply, and the state its change is in.
	unusable := func(t *testing.T, h held, state string) {
		t.Helper()
		if s := e.changeState(t, h.change); s != state {
			t.Errorf("change state = %s, want %s", s, state)
		}
		var rows, bindings int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM delegates WHERE id = $1 OR issuer = $2`, h.id, h.issuer).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM portal_issuers WHERE issuer_key = $1`, types.PortalIssuerKey(h.issuer)).Scan(&bindings); err != nil {
			t.Fatal(err)
		}
		if rows != 0 || bindings != 0 {
			t.Errorf("a registration that did not apply left %d portal row(s) and %d binding(s)", rows, bindings)
		}
		e.credentialUnknown(t, h.cred)
		if w := e.exchange(t, h.id, h.cred, "x"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_client") {
			t.Errorf("exchange with the credential = %d %s, want 401 invalid_client", w.Code, w.Body.String())
		}
	}

	t.Run("pending", func(t *testing.T) {
		h := propose(t, nil)
		unusable(t, h, types.GovernanceChangePending)
		body := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/governance/changes/"+h.change, e.admin2, "").Body.String()
		if strings.Contains(body, h.cred) || strings.Contains(body, hashPortalCredential(h.cred)) {
			t.Errorf("the change read carries the credential or its digest: %s", body)
		}
	})
	t.Run("self-approval", func(t *testing.T) {
		h := propose(t, nil)
		if w := decide(e.admin, h.change, "approve"); w.Code == http.StatusOK {
			t.Errorf("the proposer approved their own registration: %s", w.Body.String())
		}
		unusable(t, h, types.GovernanceChangePending)
	})
	t.Run("withdraw", func(t *testing.T) {
		h := propose(t, nil)
		if w := decide(e.admin, h.change, "reject"); w.Code != http.StatusOK {
			t.Fatalf("withdraw = %d %s", w.Code, w.Body.String())
		}
		if w := decide(e.admin2, h.change, "approve"); w.Code == http.StatusOK {
			t.Error("a withdrawn registration was approved")
		}
		unusable(t, h, types.GovernanceChangeRejected) // a withdrawal is the proposer's own reject
	})
	t.Run("rejection", func(t *testing.T) {
		h := propose(t, nil)
		if w := decide(e.admin2, h.change, "reject"); w.Code != http.StatusOK {
			t.Fatalf("reject = %d %s", w.Code, w.Body.String())
		}
		unusable(t, h, types.GovernanceChangeRejected)
	})
	t.Run("forced commit failure", func(t *testing.T) {
		h := propose(t, nil)
		// A deferred constraint trigger fails at COMMIT, after the decision has written the binding, the portal
		// row and the change's new state: the commit itself fails and every one of those writes is undone.
		for _, stmt := range []string{
			`CREATE OR REPLACE FUNCTION regtest_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced'; END $$`,
			`CREATE CONSTRAINT TRIGGER regtest_fail AFTER INSERT ON delegates DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION regtest_fail()`,
		} {
			if _, err := e.pool.Exec(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
		w := decide(e.admin2, h.change, "approve")
		if _, err := e.pool.Exec(ctx, `DROP TRIGGER regtest_fail ON delegates`); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusInternalServerError {
			t.Errorf("approve with a failing commit = %d %s, want 500", w.Code, w.Body.String())
		}
		unusable(t, h, types.GovernanceChangePending)
	})
	t.Run("census dirty at approval", func(t *testing.T) {
		h := propose(t, map[string]any{"issuer": "https://held-dirty.example.com"})
		e.orphan(t, h.issuer)
		if w := decide(e.admin2, h.change, "approve"); w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateNamespaceInUse {
			t.Errorf("approve = %d %s, want 409 %s", w.Code, w.Body.String(), reasonDelegateNamespaceInUse)
		}
		unusable(t, h, types.GovernanceChangePending)
	})
	t.Run("pin unit deleted", func(t *testing.T) {
		unit := uuid.New()
		if _, err := e.pool.Exec(ctx, `INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}','t')`, unit, "held-pin-"+unit.String()); err != nil {
			t.Fatal(err)
		}
		h := propose(t, map[string]any{"run_in_under": unit.String()})
		if _, err := e.pool.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, unit); err != nil {
			t.Fatal(err)
		}
		if w := decide(e.admin2, h.change, "approve"); w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateRunInUnderInvalid {
			t.Errorf("approve = %d %s, want 409 %s", w.Code, w.Body.String(), reasonDelegateRunInUnderInvalid)
		}
		unusable(t, h, types.GovernanceChangePending)
	})
	t.Run("user type deleted", func(t *testing.T) {
		ut := "held-ut-" + strings.ToLower(uuid.NewString()[:8])
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: ut, Name: ut}); err != nil {
			t.Fatal(err)
		}
		h := propose(t, map[string]any{"user_type": ut})
		if err := e.st.DeleteUserType(ctx, ut); err != nil {
			t.Fatal(err)
		}
		if w := decide(e.admin2, h.change, "approve"); w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateUserTypeInvalid {
			t.Errorf("approve = %d %s, want 409 %s", w.Code, w.Body.String(), reasonDelegateUserTypeInvalid)
		}
		unusable(t, h, types.GovernanceChangePending)
	})
	t.Run("internal host delisted", func(t *testing.T) {
		h := propose(t, map[string]any{"internal_issuer": "https://idp.internal.example.com"})
		t.Setenv(envPortalInternalIssuerHosts, "")
		if w := decide(e.admin2, h.change, "approve"); w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateInternalIssuerNotListed {
			t.Errorf("approve = %d %s, want 409 %s", w.Code, w.Body.String(), reasonDelegateInternalIssuerNotListed)
		}
		unusable(t, h, types.GovernanceChangePending)
	})
	t.Run("approved by a distinct super admin", func(t *testing.T) {
		h := propose(t, nil)
		if w := decide(e.admin2, h.change, "approve"); w.Code != http.StatusOK {
			t.Fatalf("approve = %d %s", w.Code, w.Body.String())
		}
		if s := e.changeState(t, h.change); s != types.GovernanceChangeApplied {
			t.Errorf("state = %s, want %s", s, types.GovernanceChangeApplied)
		}
		d, err := e.st.GetIssuerDelegateByRaw(ctx, h.cred)
		if err != nil || d.ID != h.id || d.IssuerKey != types.PortalIssuerKey(h.issuer) {
			t.Fatalf("the approved portal = %+v, %v", d, err)
		}
		if got := e.h.auditOutcomes("delegate.create"); len(got) == 0 {
			t.Error("the approval wrote no delegate.create row")
		}
	})
}
