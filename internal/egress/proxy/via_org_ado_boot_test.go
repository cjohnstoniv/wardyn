// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A host this proxy brokers as a git_pat grant is no organisation route: a /wardyn/git/ request for it is served
// here, though a via_org route names the same host. Without the guard that request would leave for the organisation.
func TestLaptopProxy_ViaOrgADOGitStaysLocalWithAGitPATGrant(t *testing.T) {
	f := newLaptopFixture(t)
	f.p.viaOrgRoutes = append(f.p.viaOrgRoutes, ViaOrgRoute{GrantID: uuid.New(), Host: viaOrgADOHost})
	f.p.patGrants = map[string]PATGrant{viaOrgADOHost: {GrantID: uuid.New()}}
	w := httptest.NewRecorder()
	f.p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, viaOrgADOInfo, nil))
	// The local broker's own mint call reaches the control plane too; only the via_org route's path is the forward.
	if w.Body.String() == "from-org" && f.cpReq != nil && strings.HasPrefix(f.cpReq.URL.Path, ViaOrgPathPrefix) {
		t.Fatalf("the request was forwarded to the organisation (%q), want this proxy to broker the git_pat host itself", w.Body)
	}
	if f.cpReq != nil && strings.HasPrefix(f.cpReq.URL.Path, ViaOrgPathPrefix) {
		t.Fatalf("the organisation's via_org route saw %s, want none", f.cpReq.URL.Path)
	}
}

// An engine's Azure DevOps credential is resolved at its first request, not at boot: a boot resolve has no re-auth
// hold, so a sign-in that lapsed while the run idled would fail the engine's rebuild and take the run's other via_org
// grants down with it. Every other injection still resolves at boot.
func TestBuildInjector_LazyADOCredentialForAnEngine(t *testing.T) {
	org := &fakeOrg{}
	ado, other := uuid.New(), uuid.New()
	rules := []InjectionConfig{
		{GrantID: ado, InjectionRule: egress.InjectionRule{Host: viaOrgADOHost, Header: "Authorization", SecretName: types.ADOEntraAccessTokenSecret}},
		{GrantID: other, InjectionRule: egress.InjectionRule{Host: viaOrgHost, Header: "Authorization", SecretName: "corp-secret"}},
	}
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{viaOrgADOHost, viaOrgHost}})
	client := &http.Client{Transport: org}
	org.failGrant = ado // the sign-in has lapsed: the credential's resolve fails

	if _, err := buildInjectorFor(t.Context(), uuid.Nil, false, "https://wardynd.test", newTokenSource("tok"), pol, rules, client); err == nil {
		t.Fatal("a sidecar's boot resolve of a lapsed credential built an injector, want it to fail closed")
	}
	inj, err := buildInjectorFor(t.Context(), uuid.Nil, true, "https://wardynd.test", newTokenSource("tok"), pol, rules, client)
	if err != nil {
		t.Fatalf("an engine's build failed on a lapsed Azure DevOps credential: %v", err)
	}
	if org.injections != 1 {
		t.Fatalf("boot resolved %d credentials, want 1: only the non-Azure DevOps one", org.injections)
	}
	org.failGrant = uuid.Nil // the owner signed in again
	h, ok, err := inj.resolve(viaOrgADOHost)
	if err != nil || !ok || h.value != viaOrgSecret {
		t.Fatalf("first use: header %q ok %v err %v, want the credential resolved then", h.value, ok, err)
	}
	if org.injections != 2 {
		t.Fatalf("%d resolves after first use, want 2", org.injections)
	}
}
