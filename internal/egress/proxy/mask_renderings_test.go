// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F155 — the proxy-side mask registers per RENDERING, not per credential.
//
// procRegistry is what stands between a proxy-held credential and every
// sandbox-facing error body (Proxy.httpError -> maskDecisionBytes) and every
// decision-log line, and secretmask.Masker.Mask is exact bytes. So a credential
// is protected in exactly the renderings that were registered — and the three
// registration sites used to disagree about what that means:
//
//   - inject.go registered ONLY the FORMATTED header value ("Bearer sk-…"),
//     leaving the bare token — the form a vendor echoes in an error body — open;
//   - git_broker.go registered ONLY the raw installation token, while the token
//     goes on the wire as base64("x-access-token:" + tok);
//   - pat_broker.go registered NOTHING at all;
//   - upstream.go (same package) already registered all three renderings of the
//     corp-proxy credential, which is the shape the others now share.
//
// Each case below masks a buffer holding ONE rendering of a credential the
// proxy has just taken possession of. maskDecisionBytes is the exact function
// httpError and the decision log call, so a rendering that survives it is a
// rendering that can leave the process.

// assertMasked fails when want is still present verbatim after masking.
func assertMasked(t *testing.T, rendering, what string) {
	t.Helper()
	got := maskDecisionBytes([]byte("proxy error quoting the request: " + rendering + " (end)"))
	if bytes.Contains(got, []byte(rendering)) {
		t.Fatalf("%s survived maskDecisionBytes verbatim: %q\n"+
			"the mask protects exactly the RENDERINGS that were registered, and this one "+
			"reaches the sandbox through Proxy.httpError and the decision log", what, got)
	}
}

// TestInjectionMaskCoversTheBareTokenRendering: buildInjector holds a formatted
// header value ("Bearer <tok>"); the BARE <tok> in the same buffer must be
// masked too.
func TestInjectionMaskCoversTheBareTokenRendering(t *testing.T) {
	const raw = "sk-f155-bare-injection-token-9d2c4b"
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Host: "api.f155.test", Header: "Authorization", Value: "Bearer " + raw, JTI: uuid.NewString(),
		})
	}))
	defer cp.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.f155.test"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "api.f155.test", Header: "Authorization", Format: "Bearer %s"},
		GrantID:       uuid.New(),
	}}
	if _, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client()); err != nil {
		t.Fatalf("buildInjector: %v", err)
	}

	// The rendering that was always registered stays masked (no regression)...
	assertMasked(t, "Bearer "+raw, "the formatted injection value")
	// ...and the bare credential, which is what a vendor echoes back, now is too.
	assertMasked(t, raw, "the bare injection token")
}

// TestGitBrokerMaskCoversTheBasicWireRendering: gitToken holds the raw
// installation token; the base64("x-access-token:"+tok) that SetBasicAuth
// actually puts on the wire must be masked too.
func TestGitBrokerMaskCoversTheBasicWireRendering(t *testing.T) {
	const raw = "ghs_f155instwireform4a7e1c9b3d5f"
	grantID := uuid.New()
	up := newGitBrokerUpstream(t, raw)
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": grantID}, upstreamAddr(up.srv))

	tok, err := p.gitToken(context.Background(), grantID)
	if err != nil {
		t.Fatalf("gitToken: %v", err)
	}
	if tok != raw {
		t.Fatalf("gitToken = %q, want %q", tok, raw)
	}

	assertMasked(t, raw, "the raw installation token")
	wire := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + raw))
	assertMasked(t, wire, "the base64 Basic-auth wire rendering of the installation token")
}

// TestPATBrokerMaskCoversItsToken: patToken registered NOTHING, so both the raw
// PAT and its Basic wire rendering rode out unmasked. Same root cause, same one
// definition (registerBasicAuthCredential).
func TestPATBrokerMaskCoversItsToken(t *testing.T) {
	const raw = "f155-pat-lane-token-unregistered-6b2f"
	grantID := uuid.New()
	up := newPATBrokerUpstream(t, raw, "oauth2")
	p, _ := newPATBrokerProxy(t, map[string]PATGrant{
		"gitlab.f155.test": {GrantID: grantID, Username: "oauth2"},
	}, upstreamAddr(up.srv))

	tok, user, err := p.patToken(context.Background(), PATGrant{GrantID: grantID, Username: "oauth2"})
	if err != nil {
		t.Fatalf("patToken: %v", err)
	}
	if tok != raw {
		t.Fatalf("patToken = %q, want %q", tok, raw)
	}

	assertMasked(t, raw, "the raw minted PAT")
	assertMasked(t, base64.StdEncoding.EncodeToString([]byte(user+":"+raw)),
		"the base64 Basic-auth wire rendering of the PAT")
}
