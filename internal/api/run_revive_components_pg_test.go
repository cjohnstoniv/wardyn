// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A run's components over a real Postgres: the gate's snapshot, dispatch's
// interception entry, the revive from the stored config, the re-check of the
// doors once the content is erased, and the shared sink read. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lostComponentRun is a run of owner's, lost to an outage, with a stored proxy
// config that carries one component header host: its allowlist entry, its
// injection rule (backed by a grant row whose scope is scope) and its
// interception entry.
func lostComponentRun(t *testing.T, h *harness, owner, host, scope string) (types.AgentRun, *pgReviveRunner) {
	t.Helper()
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != types.RunRunning {
		if ok, err := pg.UpdateRunStateIf(ctx, run.ID, run.State, types.RunRunning); err != nil || !ok {
			t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
		}
	}
	if err := pg.SetSandboxRef(ctx, run.ID, "wardyn-agent-"+run.ID.String()); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.MarkRunLost(ctx, run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	g, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(scope)}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{
		RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081",
		Policy: types.RunPolicySpec{AllowedDomains: []string{host}},
		Injection: []runner.InjectionGrant{{GrantID: g.ID, Rule: egress.InjectionRule{
			Host: host, Header: "Authorization", Format: "Bearer %s", RequireTLS: true}}},
		MITMHosts:     []string{host + ":443"},
		MITMCACertPEM: "ca-cert", MITMCAKeyPEM: "ca-key",
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.RunConfigKey = make([]byte, 32)
	if err := h.srv.storeRunProxyConfig(ctx, run.ID, cfg); err != nil {
		t.Fatal(err)
	}
	rn := &pgReviveRunner{fakeRunner: &fakeRunner{}}
	h.srv.cfg.Runner = rn
	h.srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080"
	return run, rn
}

// reviveReason POSTs the revive as the owner's member session, or as the
// admin token, and returns the status and the wire reason.
func reviveReason(t *testing.T, h *harness, run types.AgentRun, asOwner bool) (int, string) {
	t.Helper()
	path := "/api/v1/runs/" + run.ID.String() + "/revive"
	var w *httptest.ResponseRecorder
	if asOwner {
		w = doSSO(t, h.srv, http.MethodPost, path, ssoSession(t, run.CreatedBy, ownerEmail, oidc.RoleUser), "")
	} else {
		w = do(t, h.srv, http.MethodPost, path, adminToken, "")
	}
	return w.Code, errorReasonOf(w.Body.String())
}

const pgOrgScope = `{"host":"org-api.example","require_tls":true,"secret_name":"org-tool-token","shared":true}`

// An organisation's component is deleted after a run launched with it, and the
// run is then lost: the revive is refused for its owner and for an admin, the
// proxy is not replaced, and so neither the interception entry nor the
// credential the organisation provided is restored. Its grant rows are still
// in place — only the component is gone. A second run whose component still
// exists revives with the entry restored from its stored config.
func TestPG_Revive_ADeletedOrganisationComponentRefusesTheRevive(t *testing.T) {
	const owner = "sub-pg-org-component"
	h, sec := newRunOwnerPGHarness(t)
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	if err := sec.Put(ctx, "org-tool-token", []byte("operator-token-value")); err != nil {
		t.Fatal(err)
	}
	def := types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{{SecretName: "org-tool-token", Shared: true,
		Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}}
	newOrg := func(name string) types.Component {
		c, err := pg.CreateComponent(ctx, types.Component{ID: uuid.New(), Name: name, Definition: def, CreatedBy: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		// Nobody until granted, then granted to the owner: as the admin routes leave it.
		if err := pg.SetCapabilityRestriction(ctx, capComponent, c.ID.String(), true, "admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectUser, owner, capComponent, c.ID.String(), types.CapabilityAllow)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	launch := func(c types.Component) (types.AgentRun, *pgReviveRunner) {
		run, rn := lostComponentRun(t, h, owner, "org-api.example", pgOrgScope)
		if err := pg.PutRunComponents(ctx, run.ID, []types.RunComponent{{ComponentID: &c.ID, Name: c.Name, Version: c.Version, Definition: c.Definition}}); err != nil {
			t.Fatal(err)
		}
		return run, rn
	}

	gone := newOrg("Gone Tool")
	run, rn := launch(gone)
	if _, err := pg.DeleteComponent(ctx, gone.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, asOwner := range []bool{true, false} {
		code, reason := reviveReason(t, h, run, asOwner)
		if code != http.StatusConflict || reason != reasonOwnerComponentGone || rn.replaced != 0 {
			t.Fatalf("revive (owner %v) after the component was deleted = %d %s (replaced %d), want 409 %s and no new proxy",
				asOwner, code, reason, rn.replaced, reasonOwnerComponentGone)
		}
	}
	if r, err := pg.GetRun(ctx, run.ID); err != nil || r.LostAt == nil {
		t.Errorf("the run after the refusals = %+v, %v, want intact and still lost", r, err)
	}
	if rows, err := pg.ListRunComponents(ctx, run.ID); err != nil || len(rows) != 1 || rows[0].ComponentID == nil {
		t.Errorf("the run's snapshot = %+v, %v, want its one row kept", rows, err)
	}

	kept := newOrg("Kept Tool")
	run, rn = launch(kept)
	if code, reason := reviveReason(t, h, run, true); code != http.StatusOK || rn.replaced != 1 {
		t.Fatalf("revive with the component in place = %d %s (replaced %d), want 200", code, reason, rn.replaced)
	}
	cfg, err := proxy.LoadConfigBytes(rn.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.MITMHosts, []string{"org-api.example:443"}) || len(cfg.Injection) != 1 || cfg.Injection[0].Host != "org-api.example" {
		t.Errorf("revived config: MITMHosts %v, injections %+v; want the stored entry and rule restored", cfg.MITMHosts, cfg.Injection)
	}
}

// A run with no run_components rows — launched without components, or before
// they existed — revives as it always did, whatever the component permissions
// say now.
func TestPG_Revive_ARunWithoutComponentRowsRevivesAsBefore(t *testing.T) {
	const owner = "sub-pg-legacy"
	h, sec := newRunOwnerPGHarness(t)
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	if err := sec.Put(ctx, "artifactory-token", []byte("art-test")); err != nil {
		t.Fatal(err)
	}
	run, rn := lostComponentRun(t, h, owner, "artifactory.corp.example", `{"host":"artifactory.corp.example","secret_name":"artifactory-token"}`)
	if rows, err := pg.ListRunComponents(ctx, run.ID); err != nil || len(rows) != 0 {
		t.Fatalf("fixture: snapshot = %+v, %v, want no rows", rows, err)
	}
	if _, err := pg.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectAll, "", capFeature, featureCustomComponent, types.CapabilityDeny)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.PutCapabilityEnforcement(ctx, map[string]bool{capComponent: true}); err != nil {
		t.Fatal(err)
	}
	if code, reason := reviveReason(t, h, run, true); code != http.StatusOK || rn.replaced != 1 {
		t.Fatalf("revive of a run without components = %d %s (replaced %d), want 200", code, reason, rn.replaced)
	}
	cfg, err := proxy.LoadConfigBytes(rn.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.MITMHosts, []string{"artifactory.corp.example:443"}) {
		t.Errorf("revived MITMHosts = %v, want the stored entry", cfg.MITMHosts)
	}
}

// The whole path for a person's own component, through the real doors: the
// gate admits it and writes the snapshot, dispatch intercepts its header host
// on :443 under a per-run CA, a revive restores that from the stored config,
// and once the person's components are erased and the feature is denied the
// run can no longer be revived — though its snapshot row now says nothing of
// what the component was.
func TestPG_Dispatch_APersonsComponentFromLaunchToARefusedRevive(t *testing.T) {
	const owner, host = "sub-pg-person-component", "person-api.example"
	h, _ := newRunOwnerPGHarness(t)
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	rn := &pgReviveRunner{fakeRunner: &fakeRunner{}}
	h.srv.cfg.Runner = rn
	h.srv.cfg.RunConfigKey = make([]byte, 32)
	h.srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080" // a hop the proxy config loader accepts
	session := ssoSession(t, owner, ownerEmail, oidc.RoleUser)
	if w := doSSO(t, h.srv, http.MethodPut, "/api/v1/secrets/person-secret", session, `{"value":"person-secret-value-0000"}`); w.Code != http.StatusNoContent {
		t.Fatalf("PUT /secrets = %d %s", w.Code, w.Body)
	}

	// An inline policy, as every member launch on this harness carries: its deployment default holds
	// a bare api_key ceiling entry, which is a ceiling and not a policy a run can launch with.
	body := `{"agent":"claude-code","task":"t","interactive":true,` +
		`"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]},` +
		`"components":[{"name":"My Tool","inline":{` +
		`"hosts":["` + host + `"],"config":{"PERSON_REGION":"person-eu"},` +
		`"secrets":[{"secret_name":"person-secret","delivery":{"mode":"header","host":"` + host + `","header":"X-Person-Token","format":"%s"}}]}}]}`
	w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", session, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body)
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	rn.waitForSandbox(t)
	rn.fakeRunner.mu.Lock()
	spec := rn.lastSpec
	rn.fakeRunner.mu.Unlock()
	if pc := spec.ProxyConfig; !slices.Equal(pc.MITMHosts, []string{host + ":443"}) || pc.MITMCACertPEM == "" ||
		len(pc.Injection) != 1 || pc.Injection[0].Rule.Host != host || !pc.Injection[0].Rule.RequireTLS {
		t.Fatalf("dispatched proxy config: MITMHosts %v, CA %t, injections %+v; want the header host on :443 under a CA with its rule",
			pc.MITMHosts, pc.MITMCACertPEM != "", pc.Injection)
	}
	if spec.Env["PERSON_REGION"] != "person-eu" {
		t.Errorf("Env[PERSON_REGION] = %q, want the component's config", spec.Env["PERSON_REGION"])
	}
	rows, err := pg.ListRunComponents(ctx, created.ID)
	if err != nil || len(rows) != 1 || !rows[0].SelfDefined || rows[0].Erased || rows[0].Name != "My Tool" {
		t.Fatalf("snapshot = %+v, %v, want the person's one row", rows, err)
	}

	lose := func() {
		t.Helper()
		// Dispatch settles the run RUNNING after the sandbox exists.
		deadline := time.Now().Add(5 * time.Second)
		for {
			ok, err := pg.MarkRunLost(ctx, created.ID, types.LostOutage, time.Now(), 0)
			if err != nil {
				t.Fatalf("MarkRunLost: %v", err)
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				r, _ := pg.GetRun(ctx, created.ID)
				t.Fatalf("the run never became markable as lost: %+v", r)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	run, err := pg.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Lost, then revived: the entry comes back from the stored config.
	lose()
	if code, reason := reviveReason(t, h, run, true); code != http.StatusOK || rn.replaced != 1 {
		t.Fatalf("revive = %d %s (replaced %d), want 200", code, reason, rn.replaced)
	}
	cfg, err := proxy.LoadConfigBytes(rn.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.MITMHosts, []string{host + ":443"}) || len(cfg.Injection) != 1 || cfg.MITMCACertPEM != spec.ProxyConfig.MITMCACertPEM {
		t.Errorf("revived config: MITMHosts %v, %d injections; want the dispatched entry, rule and CA", cfg.MITMHosts, len(cfg.Injection))
	}

	// Lost again; the person's components erased, then the feature denied.
	lose()
	if w := do(t, h.srv, http.MethodPost, "/api/v1/people/"+owner+"/erasure", adminToken, erasureBody("components")); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	rows, err = pg.ListRunComponents(ctx, created.ID)
	if err != nil || len(rows) != 1 || !rows[0].Erased || !rows[0].SelfDefined || rows[0].Name != "" || len(rows[0].Definition.Hosts) != 0 {
		t.Fatalf("snapshot after the erasure = %+v, %v, want one content-free row", rows, err)
	}
	if code, reason := reviveReason(t, h, run, true); code != http.StatusOK || rn.replaced != 2 {
		t.Fatalf("revive after the erasure, feature still on = %d %s (replaced %d), want 200", code, reason, rn.replaced)
	}
	lose()
	if _, err := pg.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectAll, "", capFeature, featureCustomComponent, types.CapabilityDeny)); err != nil {
		t.Fatal(err)
	}
	for _, asOwner := range []bool{true, false} {
		code, reason := reviveReason(t, h, run, asOwner)
		if code != http.StatusForbidden || reason != reasonOwnerCapabilityComponent || rn.replaced != 2 {
			t.Fatalf("revive (owner %v) after the erasure and a denied feature = %d %s (replaced %d), want 403 %s and no new proxy",
				asOwner, code, reason, rn.replaced, reasonOwnerCapabilityComponent)
		}
	}
	// No record written for the run names what the person typed, beyond the
	// run's own policy and grant rows.
	for _, ev := range h.audit.snapshot() {
		switch ev.Action {
		case "run.create", "run.component.attach", "run.egress.add", "run.revive", "authz.denied":
			if strings.Contains(string(ev.Data), host) || strings.Contains(string(ev.Data), "person-secret") ||
				strings.Contains(string(ev.Data), "X-Person-Token") || strings.Contains(string(ev.Data), "My Tool") {
				t.Errorf("%s row names the person's component content: %s", ev.Action, ev.Data)
			}
		}
	}
}

// The shared sink read against the real secret store, which honours the
// own-row-only read: the organisation's secret answers from the operator's
// namespace although the run's owner holds one of the same name, and with the
// operator's row gone nothing answers.
func TestPG_InternalInjection_SharedGrantReadsOnlyTheOperatorNamespace(t *testing.T) {
	const owner = "sub-pg-shared-sink"
	h, sec := newRunOwnerPGHarness(t)
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	if err := sec.Put(ctx, "org-tool-token", []byte("operator-token-value")); err != nil {
		t.Fatal(err)
	}
	if err := sec.For(owner).Put(ctx, "org-tool-token", []byte("member-token-value")); err != nil {
		t.Fatal(err)
	}
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunPending, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(pgOrgScope)}})
	if err != nil {
		t.Fatal(err)
	}
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-pg-shared",
		Injection: &egress.InjectionRule{Host: "org-api.example", Header: "Authorization", SecretName: "org-tool-token", Format: "Bearer %s"}}
	token := mintRunTokenAs(t, h, run.ID, owner)
	resolve := func() (int, string) {
		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), token, "")
		return rr.Code, rr.Body.String()
	}

	code, body := resolve()
	var resp injectionResponse
	_ = json.Unmarshal([]byte(body), &resp)
	if code != http.StatusOK || resp.Value != "Bearer operator-token-value" || resp.ExpiresAt == 0 {
		t.Fatalf("resolve = %d %s, want the operator's value with a stored-key lease", code, body)
	}
	var stamped bool
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "secret.read" && ev.Outcome == "success" && strings.Contains(string(ev.Data), `"secret_scope":"operator"`) {
			stamped = true
		}
	}
	if !stamped {
		t.Error("no secret.read success row stamped with the operator scope")
	}

	if err := sec.Delete(ctx, "org-tool-token"); err != nil {
		t.Fatal(err)
	}
	if code, body := resolve(); code != http.StatusFailedDependency || strings.Contains(body, "member-token-value") {
		t.Fatalf("resolve with the operator's row gone = %d %s, want 424 and never the member's own secret", code, body)
	}
	// The member's own row is untouched and still theirs.
	if v, err := sec.For(owner).Get(secretstore.WithPurpose(context.Background(), secretstore.PurposeDispatch), "org-tool-token"); err != nil || string(v) != "member-token-value" {
		t.Errorf("the member's own secret = %q, %v", v, err)
	}
}
