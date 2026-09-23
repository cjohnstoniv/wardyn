// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAllowedExactHostAcceptsAnAuthoredPort (B10-F1) pins the consumer half of
// the port-qualification contradiction. The dispatch producer writes a
// token-bearing redirect as "m.corp:443" (F106: never bare, so a literal-IP `to`
// cannot open :22 as well) while the paired injection rule host is BARE — the
// shape buildInjector's exact-allowlist binding has always required. Both halves
// were individually right; together they made buildInjector, and therefore
// NewServer, fail closed at boot.
//
// AllowedExactHost answers a HOST question ("did the operator name this exact
// host in writing, never a wildcard, never approval"), so an entry the operator
// port-qualified is the same written naming. The PORT-less contract is kept: no
// caller passes a port, and the port clamp that matters for a credential lives
// in injectableTransport (B10-F5) and mitmPortAllowed, not here.
func TestAllowedExactHostAcceptsAnAuthoredPort(t *testing.T) {
	if !CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"m.corp:443"}}).AllowedExactHost("m.corp") {
		t.Error(`AllowedExactHost("m.corp") = false for an allowlist of ["m.corp:443"]; ` +
			"the injection rule dispatch pairs with that entry can never build")
	}

	// The buildInjector-level half: the exact error cmd/wardyn-proxy turns into
	// exit 1 for every run on an estate with a corporate artifact mirror.
	mint := mintServer(t, "tok")
	defer mint.Close()
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"m.corp:443"}})
	if _, err := buildInjector(context.Background(), mint.URL, newTokenSource("tok"), pol,
		[]InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "m.corp", Header: "Authorization", SecretName: "corp-token", Format: "Bearer %s"}, GrantID: uuid.New()}},
		mint.Client()); err != nil {
		t.Fatalf("buildInjector over the shape dispatch authors: %v", err)
	}

	// A deny on a DIFFERENT port does not cancel the allow: the operator authored
	// :443 for the credential and denied only :22, so injection still builds.
	if !CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"m.corp:443"}, DeniedDomains: []string{"m.corp:22"},
	}).AllowedExactHost("m.corp") {
		t.Error(`a deny on an unrelated port must not cancel the authored allow`)
	}

	// NEGATIVE CONTROLS — the widenings this must not become. Injection
	// still requires the operator to have named the EXACT host in writing.
	for _, c := range []struct {
		name string
		spec types.RunPolicySpec
	}{
		{"wildcard-only is still not exact", types.RunPolicySpec{AllowedDomains: []string{"*.corp:443"}}},
		{"allow_all_egress alone is still not exact", types.RunPolicySpec{AllowAllEgress: true}},
		{"a denied host stays denied however it is allowed", types.RunPolicySpec{
			AllowedDomains: []string{"m.corp:443"}, DeniedDomains: []string{"m.corp"}}},
		// The asymmetry to guard: the allow side is any-port, so the deny side
		// must be able to cancel the ports it names. CompilePolicy routes a
		// port-qualified deny to deniedExactPort only, which the port-less deny
		// checks above never read — so without the per-port shadow this policy
		// would build an injector where it must fail closed, and the credential
		// would ride an https request to a port the operator denied in writing.
		{"a port-qualified deny cancels the port-qualified allow", types.RunPolicySpec{
			AllowedDomains: []string{"m.corp:443"}, DeniedDomains: []string{"m.corp:443"}}},
	} {
		if CompilePolicy(c.spec).AllowedExactHost("m.corp") {
			t.Errorf("%s: AllowedExactHost(\"m.corp\") = true, want false", c.name)
		}
	}
}

// TestNoCleartextInjectionToTheTLSConventionalPorts (B10-F5) is the other half of
// the same change, and it ships WITH it: B10-F1 alone widens this leak, because a
// port-qualified entry is exactly what AuthoredPortFor reads as the operator
// declaring the transport.
//
// injectableTransport clamped cleartext at port 443 only, so
// `allowed_domains: ["vendor.example:8443"]` plus an api_key grant handed the
// operator's credential to `POST http://vendor.example:8443/…` in the clear —
// the F110 leak one port over. The clamp now covers the TLS-conventional set
// {443, 8443, 9443} regardless of authoring; a cleartext connector on any other
// port is unchanged (port 80, or an authored port), and an https-only vendor on
// one of these ports is served by `require_tls`, which refuses rather than
// silently withholds.
func TestNoCleartextInjectionToTheTLSConventionalPorts(t *testing.T) {
	for _, port := range []string{"443", "8443", "9443"} {
		t.Run("cleartext to :"+port+" is never credentialed", func(t *testing.T) {
			cu := captureUpstream(t, false, "ok")
			p := newProxy(Options{
				RunID: uuid.New(),
				// The operator AUTHORED this exact port — which is precisely what used
				// to re-admit the credential through AuthoredPortFor.
				Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"vendor.example:" + port}}),
				Injector: staticInj(map[string]injectedHeader{"vendor.example": {name: "X-Api-Key", value: "BROKERED-KEY"}}),
				Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
				Resolver: publicResolver{},
				Dial:     redirectDial(upstreamAddr(cu.srv)),
			})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.example:"+port+"/x", `{"a":1}`))
			if !cu.reached {
				t.Fatalf("request should still be forwarded (uncredentialed), status=%d", rec.Code)
			}
			if got := cu.header.Get("X-Api-Key"); got != "" {
				t.Fatalf("upstream received the brokered credential %q IN CLEARTEXT on a sandbox-chosen "+
					"http:// request to the TLS-conventional port %s", got, port)
			}
		})
	}

	// NEGATIVE CONTROL, in the same test: an ordinary authored cleartext
	// connector on a NON-TLS-conventional port is untouched by the widened clamp.
	t.Run("an authored cleartext connector port still injects", func(t *testing.T) {
		cu := captureUpstream(t, false, "ok")
		p := newProxy(Options{
			RunID:    uuid.New(),
			Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"vendor.example:8080"}}),
			Injector: staticInj(map[string]injectedHeader{"vendor.example": {name: "X-Api-Key", value: "BROKERED-KEY"}}),
			Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
			Resolver: publicResolver{},
			Dial:     redirectDial(upstreamAddr(cu.srv)),
		})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.example:8080/x", `{"a":1}`))
		if got := cu.header.Get("X-Api-Key"); got != "BROKERED-KEY" {
			t.Fatalf("upstream X-Api-Key = %q, want the injected credential on an operator-authored cleartext port", got)
		}
	})
}
