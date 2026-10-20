// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	fillTestHost  = "api.corp.test"
	fillTestValue = "resident-fill-value"
)

// fillDispatch dispatches, through a whole local dispatch, one runner_resident
// env_secret or file_secret grant whose scope names fillTestHost, with
// WARDYN_RUNNER_SECRET_PLACEHOLDERS as given. It returns the grant, what the
// runner received (each deliver_resident and the one create_sandbox), and the
// run's grant rows afterwards.
func fillDispatch(t *testing.T, kind types.GrantKind, on bool) (types.CredentialGrant, []runnerwire.DeliverResidentArgs, json.RawMessage, []types.CredentialGrant, *residentFix) {
	t.Helper()
	f := newResidentFix(t, nil, residentAllClassesPolicy)
	f.srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
	f.srv.cfg.RunnerSecretPlaceholders = on
	// reportCaps advertises CC2 only; a run with no class asks for CC1, which
	// the create needs to reach the runner.
	caps, err := json.Marshal(runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1, types.CC2}, ManagedFiles: true}, Version: "test", Resident: true})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.runnerEndpoint(f.rn.ID).sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	ctx := t.Context()
	if err := f.cfg.Secrets.(*memSecrets).Put(ctx, "op-secret", []byte(fillTestValue)); err != nil {
		t.Fatal(err)
	}
	run := f.startingRun(t, func(r *types.AgentRun) { r.State = types.RunPending })
	scope := `{"name":"RES_TOKEN","secret_name":"op-secret","host":"` + fillTestHost + `"}`
	if kind == types.GrantFileSecret {
		scope = `{"file":"res.key","secret_name":"op-secret","host":"` + fillTestHost + `"}`
	}
	g, err := f.st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryResident,
		Spec: types.GrantSpec{Kind: kind, Scope: json.RawMessage(scope)}})
	if err != nil {
		t.Fatal(err)
	}
	policy := policyOf(g)
	policy.AllowedDomains = []string{fillTestHost}
	f.srv.dispatchRun(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()),
		dispatchParams{RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policy})
	f.srv.WaitBackground()
	for _, ev := range syncAuditRows(f.audit, "run.dispatch") {
		if ev.RunID != nil && *ev.RunID == run.ID && ev.Outcome == "failure" {
			t.Fatalf("dispatch refused the run: %s", ev.Data)
		}
	}
	delivered, _ := f.side.calls()
	f.side.mu.Lock()
	created := slices.Clone(f.side.created)
	f.side.mu.Unlock()
	if len(created) != 1 {
		var failed []string
		f.audit.mu.Lock()
		for _, ev := range f.audit.events {
			if ev.Outcome == "failure" {
				failed = append(failed, ev.Action+" "+string(ev.Data))
			}
		}
		f.audit.mu.Unlock()
		t.Fatalf("%d create_sandbox calls, want one; failures: %v", len(created), failed)
	}
	rows, err := f.st.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return g, delivered, created[0], rows, f
}

// placed is where the created spec carries the grant's value or placeholder.
func placed(t *testing.T, kind types.GrantKind, args json.RawMessage) (string, runner.ProxyConfig) {
	t.Helper()
	var a runnerwire.CreateSandboxArgs
	if err := json.Unmarshal(args, &a); err != nil {
		t.Fatal(err)
	}
	if kind == types.GrantEnvSecret {
		return a.Spec.SecretEnv["RES_TOKEN"], a.Spec.ProxyConfig
	}
	for _, mf := range a.Spec.ManagedFiles {
		if mf.Path == runner.ComponentSecretDir+"/res.key" {
			return string(mf.Content), a.Spec.ProxyConfig
		}
	}
	return "", a.Spec.ProxyConfig
}

// With WARDYN_RUNNER_SECRET_PLACEHOLDERS on, the runner never receives the
// value: no deliver_resident carries it, and the create holds the fill
// placeholder and a via_org route for the host, never the rule or the value.
// The fill grant is recorded via_org, so the organisation's egress serves it.
func TestDispatch_RunnerSecretPlaceholderKeepsTheValueOffTheRunner(t *testing.T) {
	for _, kind := range []types.GrantKind{types.GrantEnvSecret, types.GrantFileSecret} {
		t.Run(string(kind), func(t *testing.T) {
			g, delivered, created, rows, f := fillDispatch(t, kind, true)
			for _, d := range delivered {
				if d.GrantID == g.ID || d.Value == fillTestValue {
					t.Fatalf("delivered %+v, want the value kept off the runner", d)
				}
			}
			if bytes.Contains(created, []byte(fillTestValue)) {
				t.Fatalf("create_sandbox carries the value: %s", created)
			}
			got, pc := placed(t, kind, created)
			fill, ok := placement.FillGrantID(got)
			if !ok {
				t.Fatalf("runner got %q, want a fill placeholder", got)
			}
			if len(pc.Injection) != 0 || !slices.Equal(pc.ViaOrgRoutes, []proxy.ViaOrgRoute{{GrantID: fill, Host: fillTestHost}}) ||
				!slices.Contains(pc.MITMHosts, fillTestHost) || pc.MITMCAKeyPEM == "" {
				t.Fatalf("runner proxy injection %+v routes %+v mitm %v, want only the via_org route, intercepted", pc.Injection, pc.ViaOrgRoutes, pc.MITMHosts)
			}
			i := slices.IndexFunc(rows, func(r types.CredentialGrant) bool { return r.ID == fill })
			if i < 0 || rows[i].Delivery != types.GrantDeliveryViaOrg || rows[i].Spec.Kind != types.GrantAPIKey || !apiKeyScopeShared(rows[i].Spec.Scope) {
				t.Fatalf("fill grant %+v, want a shared api_key recorded via_org", rows)
			}
			if rule, err := injectionRuleFromScope(rows[i].Spec.Scope); err != nil || rule.Placeholder != got || rule.Host != fillTestHost || rule.SecretName != "op-secret" {
				t.Fatalf("fill rule %+v (%v), want the placeholder, host and secret", rule, err)
			}
			audited := false
			for _, ev := range syncAuditRows(f.audit, "run.secret_placeholder.resolve") {
				audited = audited || (ev.Outcome == "success" && strings.Contains(string(ev.Data), `"delivery":"via_org"`))
			}
			if !audited {
				t.Fatal("no run.secret_placeholder.resolve success row naming via_org")
			}
		})
	}
}

// Off (the 0.10.0 default), the grant is delivered runner_resident exactly as
// before: its value goes to the runner for the sandbox, the create holds its
// resident placeholder, and no fill grant or via_org route is authored.
func TestDispatch_RunnerSecretPlaceholderOffDeliversAsBefore(t *testing.T) {
	for _, kind := range []types.GrantKind{types.GrantEnvSecret, types.GrantFileSecret} {
		t.Run(string(kind), func(t *testing.T) {
			g, delivered, created, rows, f := fillDispatch(t, kind, false)
			if len(delivered) != 1 || delivered[0].GrantID != g.ID || delivered[0].Value != fillTestValue || !delivered[0].Sandbox {
				t.Fatalf("delivered %+v, want the value for the sandbox", delivered)
			}
			got, pc := placed(t, kind, created)
			if got != placement.ResidentPlaceholder(g.ID) || len(pc.ViaOrgRoutes) != 0 || len(rows) != 1 {
				t.Fatalf("runner got %q routes %+v grants %d, want the resident placeholder and nothing authored", got, pc.ViaOrgRoutes, len(rows))
			}
			if n := len(syncAuditRows(f.audit, "run.secret_placeholder.resolve")); n != 0 {
				t.Fatalf("%d placeholder rows with the switch off", n)
			}
		})
	}
}

// The organisation's egress fills the placeholder: the runner's proxy forwards
// the sandbox's own Authorization, which is the placeholder, and the upstream
// sees the value, read by the organisation alone.
func TestViaOrg_FillsARunnerResidentPlaceholder(t *testing.T) {
	f := &viaOrgAPIFixture{bindingFixture: newBindingFixture(t), up: newViaOrgUpstream(t)}
	f.online.Store(true)
	viaOrgAPI(t, f.srv, f.up, &f.online)
	grant := uuid.New()
	ph := placement.FillPlaceholderPrefix + grant.String()
	scope := mustJSON(map[string]any{"host": viaOrgAPIHost, "secret_name": "corp-secret", "format": "%s", "require_tls": true, "placeholder": ph, "shared": true})
	if _, err := f.st.CreateGrant(t.Context(), types.CredentialGrant{ID: grant, RunID: f.run.ID, Delivery: types.GrantDeliveryViaOrg,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope}}); err != nil {
		t.Fatal(err)
	}
	rule, err := injectionRuleFromScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.cfg.Broker = &fakeBroker{minted: broker.Minted{Kind: types.GrantAPIKey, JTI: "j", Injection: &rule}}
	storeViaOrgConfig(t, f.srv, f.run.ID, []proxy.ViaOrgRoute{{GrantID: grant, Host: viaOrgAPIHost}})
	req := httptest.NewRequest(http.MethodPost, proxy.ViaOrgPathPrefix+grant.String()+"/v1/x", strings.NewReader(`{"q":1}`))
	req.Header.Set("Authorization", "Bearer "+runToken(t, f.srv, f.run.ID))
	req.Header.Set(proxy.ViaOrgAuthorityHeader, viaOrgAPIHost)
	req.Header.Set(proxy.ViaOrgAuthorizationHeader, ph)
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: f.rn.ID}))
	w := httptest.NewRecorder()
	f.srv.relayRoutes(f.srv.InternalHandler()).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("via_org = %d %s", w.Code, w.Body)
	}
	if _, auth, _ := f.up.seen(); len(auth) != 1 || auth[0] != viaOrgAPIValue {
		t.Fatalf("upstream saw Authorization %q, want the filled value", auth)
	}
}
