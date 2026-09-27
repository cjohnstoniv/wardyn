// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPortQualifiedOnlyHostIsNotCredentialedOnPort80 pins the premise
// the port-80 injection arm has always been written under and that B10-F1
// silently removed: that a host bound for injection had a BARE allowlist entry,
// which is "silent about the port" and so reads port 80 as the default port of
// a plaintext connector the operator authored on purpose.
//
// Once AllowedExactHost accepted a port-QUALIFIED-only entry (so the injector
// could build against the "m.corp:443" shape dispatch authors), a host the
// operator named ONLY as `vendor.example:8443` bound the injection rule — and
// the port-80 arm then handed the credential to `POST http://vendor.example/`
// in the clear without ever consulting the authored port. The operator wrote
// one port down; the sandbox picked another, and got the secret anyway.
func TestPortQualifiedOnlyHostIsNotCredentialedOnPort80(t *testing.T) {
	cu := captureUpstream(t, false, "ok")
	p := newProxy(Options{
		RunID: uuid.New(),
		// The ONLY spelling of this host the operator authored is :8443 — the
		// shape an observed-egress approval writes. allow_all_egress is what
		// makes the port-80 dial reachable at all (a learning session, or any
		// run whose port-80 dial is approved at runtime).
		Policy: CompilePolicy(types.RunPolicySpec{
			AllowedDomains: []string{"vendor.example:8443"}, AllowAllEgress: true,
		}),
		Injector: staticInj(map[string]injectedHeader{"vendor.example": {name: "X-Api-Key", value: "BROKERED-KEY"}}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial:     redirectDial(upstreamAddr(cu.srv)),
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.example/x", `{"a":1}`))
	if !cu.reached {
		t.Fatalf("request should still be forwarded (uncredentialed), status=%d", rec.Code)
	}
	if got := cu.header.Get("X-Api-Key"); got != "" {
		t.Fatalf("upstream received the brokered credential %q IN CLEARTEXT on port 80 for a host the "+
			"operator authored ONLY as vendor.example:8443", got)
	}

	// NEGATIVE CONTROL, same test: the premise itself. A BARE entry IS silent
	// about the port, so the plaintext connector an operator authors on purpose
	// is credentialed on port 80 exactly as before.
	t.Run("a bare allowlist entry still injects on port 80", func(t *testing.T) {
		cu := captureUpstream(t, false, "ok")
		p := newProxy(Options{
			RunID:    uuid.New(),
			Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"connector.internal"}}),
			Injector: staticInj(map[string]injectedHeader{"connector.internal": {name: "X-Api-Key", value: "BROKERED-KEY"}}),
			Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
			Resolver: publicResolver{},
			Dial:     redirectDial(upstreamAddr(cu.srv)),
		})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://connector.internal/x", `{"a":1}`))
		if got := cu.header.Get("X-Api-Key"); got != "BROKERED-KEY" {
			t.Fatalf("upstream X-Api-Key = %q, want the injected credential on a bare-entry cleartext connector", got)
		}
	})
}

// TestAllowedExactHostHonoursAPortQualifiedWildcardDeny closes the
// asymmetry B10-F1's any-port arm left behind: it shadows a port-qualified
// EXACT deny but not a port-qualified WILDCARD one, so `deny *.corp:8443`
// could not cancel `allow m.corp:8443` at bind time — unlike AuthoredPortFor,
// which consults both. Alone it is a mis-bind (evalHost still denies the
// port); with the port-80 arm above it would be the same leak one door over.
func TestAllowedExactHostHonoursAPortQualifiedWildcardDeny(t *testing.T) {
	if CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"m.corp:8443"}, DeniedDomains: []string{"*.corp:8443"},
	}).AllowedExactHost("m.corp") {
		t.Error(`AllowedExactHost("m.corp") = true with allow ["m.corp:8443"] and deny ["*.corp:8443"]; ` +
			"a port-qualified wildcard deny must cancel the only port that was authored")
	}

	// NEGATIVE CONTROL: a wildcard deny on an UNRELATED port leaves the
	// authored allow standing, exactly as the exact-deny arm already does.
	if !CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"m.corp:443"}, DeniedDomains: []string{"*.corp:22"},
	}).AllowedExactHost("m.corp") {
		t.Error(`a wildcard deny on an unrelated port must not cancel the authored allow`)
	}
}
