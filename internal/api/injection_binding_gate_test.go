// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The proxy starts only with an injection its policy binds exactly (buildInjectorFor). A run whose
// policy allows a credential's host without binding it starts with that credential withheld, at
// dispatch and at revive, and the credential its policy binds is handed over unchanged.

const (
	gateVendor = "api.vendor.example"
	gateKept   = "kept.example"
)

// reassertRow is the run's run.ceiling.reassert row, decoded, or nil.
func reassertRow(t *testing.T, audit *recRecorder, run types.AgentRun) map[string]any {
	t.Helper()
	ev := findAudit(audit.snapshot(), run.ID, "run.ceiling.reassert", "success")
	if ev == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// gateRules is the injections on the two hosts these tests credential, in order.
func gateRules[T any](all []T, host func(T) string) []T {
	return slices.DeleteFunc(slices.Clone(all), func(in T) bool { return host(in) != gateKept && host(in) != gateVendor })
}

// assertAllBound fails unless the proxy policy binds every injection's host exactly.
func assertAllBound(t *testing.T, policy types.RunPolicySpec, injections []runner.InjectionGrant) {
	t.Helper()
	bound := proxy.CompilePolicy(policy).AllowedExactHost
	for _, in := range injections {
		if !bound(in.Rule.Host) {
			t.Errorf("injection on %s reaches a proxy whose policy %v does not bind it: the proxy refuses to start", in.Rule.Host, policy.AllowedDomains)
		}
	}
}

// With no profile and no layer, a policy that allows a credential's host only by allow_all_egress or a
// wildcard withholds it, and the row says so; a policy that lists it exactly keeps it byte for byte.
func TestUnboundInjectionWithheldWithNoLayer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy types.RunPolicySpec
		held   bool
	}{
		{"allow_all_egress", types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{gateKept}}, true},
		{"a wildcard", types.RunPolicySpec{AllowedDomains: []string{gateKept, "*.vendor.example"}}, true},
		{"another port", types.RunPolicySpec{AllowedDomains: []string{gateKept, gateVendor + ":8443"}}, false},
		{"listed exactly", types.RunPolicySpec{AllowedDomains: []string{gateKept, gateVendor}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
			kept, vendor := componentRule(gateKept), componentRule(gateVendor)
			injections := []runner.InjectionGrant{kept, vendor}
			policy := tc.policy
			var p dispatchParams
			var llm llmTransport
			var bedrock []string
			_, unbound := srv.reassertCeilingDenies(context.Background(), run, &policy, &injections, dispatchCeiling{resolved: true}, nil,
				nil, &p, map[string]string{}, &llm, &bedrock, nil)

			want := []runner.InjectionGrant{kept, vendor}
			if tc.held {
				want = want[:1]
			}
			if !reflect.DeepEqual(injections, want) {
				t.Fatalf("injections = %+v, want %+v", injections, want)
			}
			assertAllBound(t, policy, injections)
			row := reassertRow(t, audit, run)
			if !tc.held {
				if row != nil || len(unbound) != 0 {
					t.Errorf("row %v, unbound %v: a run that withheld nothing wrote a row", row, unbound)
				}
				return
			}
			if row == nil {
				t.Fatal("no run.ceiling.reassert row for a withheld credential: it went silently")
			}
			if hosts, _ := row["dropped_injection_hosts"].([]any); len(hosts) != 1 || hosts[0] != gateVendor {
				t.Errorf("dropped_injection_hosts = %v, want [%s]", row["dropped_injection_hosts"], gateVendor)
			}
			if note, _ := row["note"].(string); !strings.Contains(note, "does not allow these hosts exactly") {
				t.Errorf("note = %q, want the binding sentence", note)
			}
			if _, ok := row["boundary"]; ok {
				t.Errorf("row %v names a boundary no layer stood for", row)
			}
		})
	}
}

// Two layers that both narrow: the first keeps the credential's host exactly, and only the second, a
// port-only or wildcard allowlist, leaves it unbound. The gate reads the policy after the last.
func TestUnboundInjectionWithheldAfterTheLastLayer(t *testing.T) {
	own := []string{gateVendor, gateKept}
	first := listLayer("restriction", own)
	for name, last := range map[string][]string{
		"another port": {gateVendor + ":8443", gateKept},
		"a wildcard":   {"*.vendor.example", gateKept},
	} {
		for _, layers := range [][]sealLayer{{first}, {first, listLayer("boundary", last)}} {
			srv, _, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
			policy := types.RunPolicySpec{AllowedDomains: slices.Clone(own)}
			sealPolicy(&policy, layers)
			kept, vendor := componentRule(gateKept), componentRule(gateVendor)
			injections := []runner.InjectionGrant{kept, vendor}
			var p dispatchParams
			var llm llmTransport
			var bedrock []string
			srv.reassertCeilingDenies(context.Background(), run, &policy, &injections, dispatchCeiling{resolved: true}, layers,
				nil, &p, map[string]string{}, &llm, &bedrock, nil)

			if len(layers) == 1 {
				if !reflect.DeepEqual(injections, []runner.InjectionGrant{kept, vendor}) || reassertRow(t, audit, run) != nil {
					t.Errorf("%s, first layer only: injections %+v; want both kept and no row", name, injections)
				}
				continue
			}
			if !reflect.DeepEqual(injections, []runner.InjectionGrant{kept}) {
				t.Errorf("%s: injections %+v, want only %s's, unchanged", name, injections, gateKept)
			}
			assertAllBound(t, policy, injections)
			if row := reassertRow(t, audit, run); row == nil || row["boundary"] != true ||
				!slices.Contains(row["dropped_injection_hosts"].([]any), any(gateVendor)) {
				t.Errorf("%s: row %v, want boundary and %s named", name, row, gateVendor)
			}
		}
	}
}

// Through dispatch, under a hard_limits boundary whose allowlist names the credential's host only on
// another port or under a wildcard: the run starts, the withheld credential is gone, the other is
// handed over unchanged, every credential left is bound, and the row names the boundary.
func TestUnboundInjectionWithheldAtDispatchUnderTheBoundary(t *testing.T) {
	for name, list := range map[string][]string{
		"another port": {gateVendor + ":8443", gateKept},
		"a wildcard":   {"*.vendor.example", gateKept},
	} {
		t.Run(name, func(t *testing.T) {
			fr := &fakeRunner{}
			srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
			srv.cfg.Store = boundaryGrantRecStore{grantRecStore: &grantRecStore{dispatchTestStore: st},
				b: types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AllowedDomains: &list}}}
			kept, vendor := componentRule(gateKept), componentRule(gateVendor)
			srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
				RunToken: "run-token", Image: "wardyn/claude-code:latest",
				Policy:     types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{gateVendor, gateKept}},
				Injections: []runner.InjectionGrant{vendor, kept},
			})
			if fr.createCalls != 1 {
				t.Fatalf("CreateSandbox calls = %d, want 1: the run did not start; events %s", fr.createCalls, auditDump(audit.snapshot(), run.ID))
			}
			pc := fr.lastSpec.ProxyConfig
			if got := gateRules(pc.Injection, func(in runner.InjectionGrant) string { return in.Rule.Host }); !reflect.DeepEqual(got, []runner.InjectionGrant{kept}) {
				t.Errorf("injections = %+v, want only %s's, unchanged", pc.Injection, gateKept)
			}
			assertAllBound(t, pc.Policy, pc.Injection)
			if row := reassertRow(t, audit, run); row == nil || row["boundary"] != true ||
				!slices.Contains(row["dropped_injection_hosts"].([]any), any(gateVendor)) {
				t.Errorf("row %v, want boundary and %s named", row, gateVendor)
			}
		})
	}
}

// The binding check does not hide a second credential on one host: one spelled with a port, which the
// proxy could never bind, still fails the run closed as two credentials for one host.
func TestUnboundInjectionStillCountsAsACollision(t *testing.T) {
	first, other := componentRule(gateKept), componentRule(gateKept+":8443")
	st, _, fr, _ := dispatchComponents(t, dispatchParams{
		Policy:     types.RunPolicySpec{AllowedDomains: []string{gateKept}},
		Injections: []runner.InjectionGrant{first, other},
	})
	if fr.createCalls != 0 || st.State() != types.RunFailed || st.FailureHint() != credentialHostCollisionHint {
		t.Fatalf("calls %d, state %q, hint %q: want the run failed closed for two credentials on one host", fr.createCalls, st.State(), st.FailureHint())
	}
}

// At revive, under a boundary whose allowlist names the credential's host only on another port or under
// a wildcard: the revive succeeds, the withheld credential is gone and named on the row, and the other
// is the frozen one unchanged. With no layer, a frozen config whose policy binds every host keeps them.
func TestUnboundInjectionWithheldAtRevive(t *testing.T) {
	freeze := func(t *testing.T, f *reviveFixture) proxy.InjectionConfig {
		var kept proxy.InjectionConfig
		f.editConfig(t, func(c *proxy.Config) {
			c.Policy.AllowedDomains = []string{gateVendor, gateKept}
			c.PATGrants = nil
			k, v := componentRule(gateKept), componentRule(gateVendor)
			kept = proxy.InjectionConfig{GrantID: k.GrantID, InjectionRule: k.Rule}
			c.Injection = []proxy.InjectionConfig{{GrantID: v.GrantID, InjectionRule: v.Rule}, kept}
		})
		frozen, err := proxy.LoadConfigBytes(f.rs.cfg) // the kept rule as the frozen config holds it
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(frozen.Injection, func(in proxy.InjectionConfig) bool { return in.GrantID == kept.GrantID })
		if i < 0 {
			t.Fatal("the frozen config lost the kept rule")
		}
		return frozen.Injection[i]
	}
	ruleHost := func(in proxy.InjectionConfig) string { return in.Host }
	for name, list := range map[string][]string{
		"another port": {gateVendor + ":8443", gateKept},
		"a wildcard":   {"*.vendor.example", gateKept},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReviveFixture(t)
			kept := freeze(t, f)
			f.srv.cfg.Store = boundaryReviveStore{reviveStore: f.rs, b: types.Boundary{Mode: types.BoundaryHardLimits,
				Spec: types.BoundarySpec{AllowedDomains: &list}}}
			if code := f.revive(t); code != http.StatusOK {
				t.Fatalf("revive: code %d, want 200", code)
			}
			cfg := f.newConfig(t)
			if got := gateRules(cfg.Injection, ruleHost); !reflect.DeepEqual(got, []proxy.InjectionConfig{kept}) {
				t.Errorf("injections = %+v, want only %s's, unchanged", cfg.Injection, gateKept)
			}
			bound := proxy.CompilePolicy(cfg.Policy).AllowedExactHost
			for _, in := range cfg.Injection {
				if !bound(in.Host) {
					t.Errorf("revived injection on %s is unbound under %v", in.Host, cfg.Policy.AllowedDomains)
				}
			}
			ev := f.audit.eventsFor(f.run.ID, "run.revive")
			if len(ev) != 1 || ev[0].Outcome != "success" {
				t.Fatalf("run.revive rows = %+v, want one success", ev)
			}
			if hosts, _ := leaseAuditData(t, ev[0])["dropped_injection_hosts"].([]any); !slices.Contains(hosts, any(gateVendor)) {
				t.Errorf("dropped_injection_hosts = %v, want %s named", hosts, gateVendor)
			}
		})
	}
	t.Run("no layer, every host bound", func(t *testing.T) {
		f := newReviveFixture(t)
		freeze(t, f)
		if code := f.revive(t); code != http.StatusOK {
			t.Fatalf("revive: code %d, want 200", code)
		}
		if cfg := f.newConfig(t); len(gateRules(cfg.Injection, ruleHost)) != 2 {
			t.Errorf("injections = %+v, want both kept", cfg.Injection)
		}
	})
}

// reassertProxyCeiling over two layers that both narrow: only the last unbinds the host, and the revive
// withholds the credential from the policy that last layer leaves. With the first alone, both stay.
func TestUnboundInjectionWithheldAtReviveAfterTheLastLayer(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	own := []string{gateVendor, gateKept}
	first := listLayer("restriction", own)
	for _, layers := range [][]sealLayer{{first}, {first, listLayer("boundary", []string{gateVendor + ":8443", gateKept})}} {
		k, v := componentRule(gateKept), componentRule(gateVendor)
		kept := proxy.InjectionConfig{GrantID: k.GrantID, InjectionRule: k.Rule}
		cfg := &proxy.Config{RunID: run.ID, Policy: types.RunPolicySpec{AllowedDomains: slices.Clone(own)},
			Injection: []proxy.InjectionConfig{{GrantID: v.GrantID, InjectionRule: v.Rule}, kept}}
		sealPolicy(&cfg.Policy, layers)
		re, rerr := reassertProxyCeiling(run, cfg, ownerCeiling{}, layers)
		if rerr != nil {
			t.Fatalf("refused: %+v", rerr)
		}
		if len(layers) == 1 {
			if len(cfg.Injection) != 2 || len(re.droppedInjection) != 0 {
				t.Errorf("first layer only: injections %+v, dropped %v; want both kept", cfg.Injection, re.droppedInjection)
			}
			continue
		}
		if !reflect.DeepEqual(cfg.Injection, []proxy.InjectionConfig{kept}) || !slices.Equal(re.droppedInjection, []string{gateVendor}) {
			t.Errorf("injections %+v, dropped %v; want only %s's kept and %s dropped", cfg.Injection, re.droppedInjection, gateKept, gateVendor)
		}
	}
}

// A revive with no ceiling deny and no layer still withholds a frozen injection its policy does not
// bind, as dispatch does, and keeps the one it binds.
func TestUnboundInjectionWithheldAtReviveWithNoLayer(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	k, v := componentRule(gateKept), componentRule(gateVendor)
	kept := proxy.InjectionConfig{GrantID: k.GrantID, InjectionRule: k.Rule}
	cfg := &proxy.Config{RunID: run.ID, Policy: types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{gateKept}},
		Injection: []proxy.InjectionConfig{{GrantID: v.GrantID, InjectionRule: v.Rule}, kept}}
	re, rerr := reassertProxyCeiling(run, cfg, ownerCeiling{}, nil)
	if rerr != nil {
		t.Fatalf("refused: %+v", rerr)
	}
	if !reflect.DeepEqual(cfg.Injection, []proxy.InjectionConfig{kept}) || !slices.Equal(re.droppedInjection, []string{gateVendor}) {
		t.Errorf("injections %+v, dropped %v; want only %s's kept and %s dropped", cfg.Injection, re.droppedInjection, gateKept, gateVendor)
	}
}

// A layer naming only another port of a host the run lists bare seals the run to neither, whether the
// run lists it or allows all egress: no b.example:8443 is added, and the proxy neither binds a credential
// to b.example nor reads 8443 as a port authored for it.
func TestSealAddsNoPortTheRunNeverReached(t *testing.T) {
	for _, layer := range []sealLayer{listLayer("restriction", []string{"b.example:8443"}), listLayer("boundary", []string{"b.example:8443"})} {
		for _, open := range []bool{false, true} {
			spec := types.RunPolicySpec{AllowAllEgress: open, AllowedDomains: []string{"b.example"}}
			sealPolicy(&spec, []sealLayer{layer})
			if spec.AllowAllEgress || slices.Contains(spec.AllowedDomains, "b.example:8443") {
				t.Errorf("%s over open=%v: sealed open %v allowed %v, want closed without b.example:8443", layer.rec.Layer, open, spec.AllowAllEgress, spec.AllowedDomains)
			}
			pol := proxy.CompilePolicy(spec)
			if pol.AllowedExactHost("b.example") || pol.AuthoredPortFor("b.example", 8443) {
				t.Errorf("%s over open=%v: the sealed policy %v still binds b.example or authors 8443", layer.rec.Layer, open, spec.AllowedDomains)
			}
		}
	}
}

// Allow-all, no seal layer, one api_key grant: the injection run create authors for the grant
// (runs_create_grants.go), on a host allow_all_egress gives no exact entry. At eb5a6eea5 this config
// reached the proxy's exact-allowlist refusal and the run never started
// (TestProxyBoot_AllowAllRefusesAnAPIKeyInjection in internal/egress/proxy). Now the run starts with
// the credential withheld and its egress still open, the row names the host, and the sidecar boots
// over the run's own dispatched config.
func TestDispatch_AllowAllAPIKeyGrantIsWithheld(t *testing.T) {
	rule, err := injectionRuleFromScope(apiKeyGrant(gateVendor, "vendor-key").Scope)
	if err != nil {
		t.Fatal(err)
	}
	_, audit, fr, run := dispatchComponents(t, dispatchParams{
		Policy:     types.RunPolicySpec{AllowAllEgress: true},
		Injections: []runner.InjectionGrant{{GrantID: uuid.New(), Rule: rule}},
	})
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1: the run did not start; events %s", fr.createCalls, auditDump(audit.snapshot(), run.ID))
	}
	pc := fr.lastSpec.ProxyConfig
	if !pc.Policy.AllowAllEgress {
		t.Error("the run's egress was closed: the gate only withholds")
	}
	for _, in := range pc.Injection {
		if in.Rule.Host == gateVendor {
			t.Errorf("the api_key credential on %s reached the proxy, whose policy does not bind it", gateVendor)
		}
	}
	assertAllBound(t, pc.Policy, pc.Injection)
	row := reassertRow(t, audit, run)
	if row == nil {
		t.Fatal("no run.ceiling.reassert row for the withheld credential: it went silently")
	}
	if hosts, _ := row["dropped_injection_hosts"].([]any); len(hosts) != 1 || hosts[0] != gateVendor {
		t.Errorf("dropped_injection_hosts = %v, want [%s]", row["dropped_injection_hosts"], gateVendor)
	}
	if note, _ := row["note"].(string); !strings.Contains(note, "does not allow these hosts exactly") {
		t.Errorf("note = %q, want the binding sentence", note)
	}

	// The sidecar boots over the config this run was dispatched with.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: "Bearer k",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}))
	t.Cleanup(cp.Close)
	pc.ControlPlaneURL = cp.URL // the harness's own is plain http to a name, which the proxy refuses
	raw, err := runner.BuildProxyConfig(fr.lastSpec.RunID, pc, runner.ProxyListenPort)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes over the dispatched config: %v", err)
	}
	cfg.Listen = "127.0.0.1:0"
	psrv, err := proxy.NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatalf("proxy.NewServer over an allow-all run's dispatched config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = psrv.Shutdown(ctx)
}
