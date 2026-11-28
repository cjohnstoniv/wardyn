// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const inboundTestOrigin = "https://wardyn.example.com"

// inboundTestServer is a server with only the configuration validateInboundTrust reads.
func inboundTestServer(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// okInboundTrust is a create body every check accepts on a server whose RunnerOrgURL is inboundTestOrigin.
func okInboundTrust() inboundTrustRequest {
	return inboundTrustRequest{Name: "gh-deploy", Issuer: "https://token.actions.githubusercontent.com",
		Audience: inboundTestOrigin, Subject: "repo:org/app:ref:refs/heads/main", ServiceIdentity: "ci-bot"}
}

// wantInboundReason fails unless validating req on s gives reason ("" for accepted).
func wantInboundReason(t *testing.T, s *Server, what string, req inboundTrustRequest, reason string) string {
	t.Helper()
	_, got, msg := s.validateInboundTrust(req)
	if got != reason {
		t.Errorf("%s: reason %q (%s), want %q", what, got, msg, reason)
	}
	return msg
}

// TestInboundTrust_TargetsServiceIdentityOnly: a trust names a service identity by its name, never a person, a
// group or a role.
func TestInboundTrust_TargetsServiceIdentityOnly(t *testing.T) {
	s := inboundTestServer(Config{RunnerOrgURL: inboundTestOrigin})
	wantInboundReason(t, s, "a service name", okInboundTrust(), "")
	for _, id := range []string{"user:x", "group:x", "admin", "", "Svc"} {
		req := okInboundTrust()
		req.ServiceIdentity = id
		wantInboundReason(t, s, "service_identity "+id, req, reasonInboundTrustInvalid)
	}
}

// TestInboundTrust_RefusesWildcardSubject: the match is exact, so a wildcard, a control character, invalid UTF-8
// and an over-long value are refused in the subject and the audience.
func TestInboundTrust_RefusesWildcardSubject(t *testing.T) {
	s := inboundTestServer(Config{RunnerOrgURL: inboundTestOrigin})
	for _, bad := range []string{"repo:org/*", "a\x00b", "a\nb", "\xff\xfe", strings.Repeat("a", 513)} {
		req := okInboundTrust()
		req.Subject = bad
		wantInboundReason(t, s, "subject "+bad, req, reasonInboundTrustInvalid)
		req = okInboundTrust()
		req.Audience = bad
		wantInboundReason(t, s, "audience "+bad, req, reasonInboundTrustInvalid)
	}
	req := okInboundTrust()
	req.Subject = strings.Repeat("a", 512)
	wantInboundReason(t, s, "a 512-byte subject", req, "")
	req = okInboundTrust()
	req.RequiredClaim, req.RequiredClaimValue = strings.Repeat("c", 257), "v"
	wantInboundReason(t, s, "a 257-byte claim name", req, reasonInboundTrustInvalid)
}

// TestInboundTrust_RefusesWardynOwnIssuers: an issuer on any host this Wardyn serves, a per-run UI host included,
// is reserved; a malformed issuer is invalid.
func TestInboundTrust_RefusesWardynOwnIssuers(t *testing.T) {
	s := inboundTestServer(Config{RunnerOrgURL: inboundTestOrigin, OIDCRedirectURL: "https://console.example.com/auth/callback",
		UIAdvertiseURL: "https://ui.example.com:8443", ControlPlaneURL: "https://wardynd:8443"})
	for _, iss := range []string{"https://wardyn.example.com/idp", "https://CONSOLE.example.com", "https://ui.example.com", "https://wardynd"} {
		req := okInboundTrust()
		req.Issuer = iss
		wantInboundReason(t, s, "own issuer "+iss, req, reasonInboundTrustReserved)
	}
	for _, iss := range []string{"spiffe://td", "http://idp.example.com", "https://idp.example.com?x=1",
		"https://idp.example.com#f", "https://u:p@idp.example.com", "https://idp.example.com/\xff"} {
		req := okInboundTrust()
		req.Issuer = iss
		wantInboundReason(t, s, "malformed issuer "+iss, req, reasonInboundTrustInvalid)
	}
	runs := inboundTestServer(Config{RunnerOrgURL: inboundTestOrigin, UIOriginTemplate: "https://run-{run}.ui.example.com"})
	for iss, want := range map[string]string{
		"https://run-abc.ui.example.com":    reasonInboundTrustReserved,
		"https://RUN-7.UI.example.com":      reasonInboundTrustReserved,
		"https://run-.ui.example.com":       "",
		"https://abc.ui.example.com":        "",
		"https://run-abc.other.example.com": "",
	} {
		req := okInboundTrust()
		req.Issuer = iss
		wantInboundReason(t, runs, "per-run template, issuer "+iss, req, want)
	}
	path := inboundTestServer(Config{RunnerOrgURL: inboundTestOrigin, UIOriginTemplate: "https://ui.example.com/{run}"})
	req := okInboundTrust()
	req.Issuer = "https://ui.example.com"
	wantInboundReason(t, path, "path template, its host", req, reasonInboundTrustReserved)
}

// TestUIRunHost: a per-run host is the template's host with any first label where {run} stands.
func TestUIRunHost(t *testing.T) {
	cases := []struct {
		host, template string
		want           bool
	}{
		{"run-abc.ui.example.com", "https://run-{run}.ui.example.com", true},
		{"RUN-7.UI.example.com", "https://run-{run}.ui.example.com", true},
		{"run-.ui.example.com", "https://run-{run}.ui.example.com", false},
		{"abc.ui.example.com", "https://run-{run}.ui.example.com", false},
		{"run-abc.other.example.com", "https://run-{run}.ui.example.com", false},
		{"ui.example.com", "https://ui.example.com/{run}", true},
		{"other.example.com", "https://ui.example.com/{run}", false},
		{"abc-ui.example.com", "https://{run}-ui.example.com", true},
		{"-ui.example.com", "https://{run}-ui.example.com", false},
		{"run-abc.ui.example.com", "https://run-{run}.ui.example.com:8443", true},
	}
	for _, c := range cases {
		if got := uiRunHost(c.host, c.template); got != c.want {
			t.Errorf("uiRunHost(%q, %q) = %v, want %v", c.host, c.template, got, c.want)
		}
	}
	for _, host := range []string{"run-abc.ui.example.com", "ui.example.com", ""} {
		if uiRunHost(host, "") {
			t.Errorf("uiRunHost(%q, \"\") = true, want false for an empty template", host)
		}
	}
}

// TestInboundTrust_AudienceMustBeThisDeployment: an audience is one of this deployment's own origins, by exact
// bytes; the UI-sandbox origin and the internal listener are never one.
func TestInboundTrust_AudienceMustBeThisDeployment(t *testing.T) {
	s := inboundTestServer(Config{RunnerOrgURL: "https://Wardyn.Example.com:443/org",
		OIDCRedirectURL: "https://console.example.com/auth/callback", UIAdvertiseURL: "https://ui.example.com:8443"})
	for _, aud := range []string{"https://wardyn.example.com", "https://console.example.com"} {
		req := okInboundTrust()
		req.Audience = aud
		wantInboundReason(t, s, "own audience "+aud, req, "")
	}
	for _, aud := range []string{"https://ui.example.com:8443", "https://wardyn.example.com/", "https://wardyn.example.com/org",
		"https://wardyn.example.com:444", "http://wardyn.example.com", "https://WARDYN.example.com", "https://ui.example.com",
		"https://other.example.com"} {
		req := okInboundTrust()
		req.Audience = aud
		msg := wantInboundReason(t, s, "foreign audience "+aud, req, reasonInboundTrustReserved)
		if !strings.HasSuffix(msg, ": https://wardyn.example.com, https://console.example.com") {
			t.Errorf("foreign audience %s: message %q, want it to list only the two own origins", aud, msg)
		}
	}
	for _, ui := range []string{"", "https://ui.example.com:8443"} {
		none := inboundTestServer(Config{UIAdvertiseURL: ui})
		for _, aud := range []string{inboundTestOrigin, "https://ui.example.com:8443"} {
			req := okInboundTrust()
			req.Audience = aud
			msg := wantInboundReason(t, none, "no public address, audience "+aud, req, reasonInboundTrustReserved)
			if !strings.Contains(msg, "no public address configured") {
				t.Errorf("no public address: message %q, want the no-public-address sentence", msg)
			}
		}
	}
	cp := inboundTestServer(Config{ControlPlaneURL: inboundTestOrigin})
	if got := cp.ownOrigins(); len(got) != 0 {
		t.Errorf("ControlPlaneURL alone: own origins %v, want none", got)
	}
}

// TestInboundTrust_AudienceWithBasePath: behind a base path an own address is the origin plus the base path, and the
// bare origin is refused.
func TestInboundTrust_AudienceWithBasePath(t *testing.T) {
	s := inboundTestServer(Config{RunnerOrgURL: "https://wardyn.example.com/org", BasePath: "/wardyn"})
	req := okInboundTrust()
	req.Audience = "https://wardyn.example.com/wardyn"
	wantInboundReason(t, s, "origin plus base path", req, "")
	for _, aud := range []string{"https://wardyn.example.com", "https://wardyn.example.com/wardyn/", "https://wardyn.example.com/org"} {
		req := okInboundTrust()
		req.Audience = aud
		msg := wantInboundReason(t, s, "base path, audience "+aud, req, reasonInboundTrustReserved)
		if !strings.HasSuffix(msg, ": https://wardyn.example.com/wardyn") {
			t.Errorf("base path, audience %s: message %q, want it to list the origin plus the base path", aud, msg)
		}
	}
}

// TestInboundTrust_RefusesSharedAudience: a provider's shared default is refused even where it is an own origin.
func TestInboundTrust_RefusesSharedAudience(t *testing.T) {
	for _, aud := range append(slicesClone(inboundSharedAudiences), "https://github.com/org") {
		if !inboundSharedAudience(aud, "https://idp.example.com") {
			t.Errorf("inboundSharedAudience(%q) = false, want true", aud)
		}
	}
	if !inboundSharedAudience("https://idp.example.com", "https://idp.example.com") {
		t.Error("an audience equal to the issuer is not refused as shared")
	}
	if inboundSharedAudience(inboundTestOrigin, "https://idp.example.com") {
		t.Errorf("inboundSharedAudience(%q) = true, want false", inboundTestOrigin)
	}
	s := inboundTestServer(Config{RunnerOrgURL: "https://kubernetes.default.svc"})
	req := okInboundTrust()
	req.Audience = "https://kubernetes.default.svc"
	if msg := wantInboundReason(t, s, "a shared default that is an own origin", req, reasonInboundTrustReserved); !strings.Contains(msg, "shared default") {
		t.Errorf("shared default: message %q, want the shared-default sentence", msg)
	}
}

func slicesClone(in []string) []string { return append([]string(nil), in...) }

// TestInboundTrust_RefusesConsoleAndPortalAudience: the console sign-in client's id is never an audience, even when
// it equals an own origin. The portal half is the store's.
func TestInboundTrust_RefusesConsoleAndPortalAudience(t *testing.T) {
	auth := newAccessAuth(t, nil, oidc.RoleUser, nil, nil, func(c *oidc.Config) { c.ClientID = inboundTestOrigin })
	s := inboundTestServer(Config{RunnerOrgURL: inboundTestOrigin, OIDC: auth})
	if msg := wantInboundReason(t, s, "the console client id", okInboundTrust(), reasonInboundTrustReserved); !strings.Contains(msg, "console sign-in client") {
		t.Errorf("console client: message %q, want the console sentence", msg)
	}
}

// TestInboundTrust_Bounds: 0 is the default; the bounds are inclusive; the extra claim is a pair and never a
// registered claim.
func TestInboundTrust_Bounds(t *testing.T) {
	s := inboundTestServer(Config{RunnerOrgURL: inboundTestOrigin})
	tr, reason, msg := s.validateInboundTrust(okInboundTrust())
	if reason != "" || tr.TokenTTLSeconds != types.InboundTrustTTLDefaultSeconds || tr.MaxTokenAgeSeconds != 300 || tr.MaxLive != 100 {
		t.Fatalf("defaults = %+v (%s %s), want 900 / 300 / 100", tr, reason, msg)
	}
	set := func(f func(*inboundTrustRequest)) inboundTrustRequest {
		req := okInboundTrust()
		f(&req)
		return req
	}
	for name, req := range map[string]inboundTrustRequest{
		"ttl 59":        set(func(r *inboundTrustRequest) { r.TokenTTLSeconds = 59 }),
		"ttl 3601":      set(func(r *inboundTrustRequest) { r.TokenTTLSeconds = 3601 }),
		"age 59":        set(func(r *inboundTrustRequest) { r.MaxTokenAgeSeconds = 59 }),
		"age 3601":      set(func(r *inboundTrustRequest) { r.MaxTokenAgeSeconds = 3601 }),
		"max_live -1":   set(func(r *inboundTrustRequest) { r.MaxLive = -1 }),
		"max_live 1001": set(func(r *inboundTrustRequest) { r.MaxLive = 1001 }),
		"claim alone":   set(func(r *inboundTrustRequest) { r.RequiredClaim = "repository_owner" }),
		"value alone":   set(func(r *inboundTrustRequest) { r.RequiredClaimValue = "org" }),
		"claim sub":     set(func(r *inboundTrustRequest) { r.RequiredClaim, r.RequiredClaimValue = "sub", "x" }),
	} {
		wantInboundReason(t, s, name, req, reasonInboundTrustInvalid)
	}
	for name, req := range map[string]inboundTrustRequest{
		"ttl 60, age 60, max_live 1":        set(func(r *inboundTrustRequest) { r.TokenTTLSeconds, r.MaxTokenAgeSeconds, r.MaxLive = 60, 60, 1 }),
		"ttl 3600, age 3600, max_live 1000": set(func(r *inboundTrustRequest) { r.TokenTTLSeconds, r.MaxTokenAgeSeconds, r.MaxLive = 3600, 3600, 1000 }),
		"a claim pair":                      set(func(r *inboundTrustRequest) { r.RequiredClaim, r.RequiredClaimValue = "repository_owner", "org" }),
	} {
		wantInboundReason(t, s, name, req, "")
	}
}

// TestServiceIdentityUpdate_AuditCountsTrustsRemoved: a sponsor change's row counts the trusts it removed.
func TestServiceIdentityUpdate_AuditCountsTrustsRemoved(t *testing.T) {
	data := serviceUpdateAuditData(store.ServiceIdentityChanged{TokensRevoked: 1, TrustsRemoved: 2})
	if data["trusts_removed"] != 2 {
		t.Errorf("service_identity.update data = %v, want trusts_removed 2", data)
	}
}

// TestURLOrigin: an origin is the lower-cased scheme and host, a bracketed IPv6 host, and a port only when it is
// not the scheme's default.
func TestURLOrigin(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Wardyn.Example.com:443/org":        "https://wardyn.example.com",
		"https://console.example.com/auth/callback": "https://console.example.com",
		"https://ui.example.com:8443":               "https://ui.example.com:8443",
		"https://wardyn.example.com:444":            "https://wardyn.example.com:444",
		"http://wardyn.example.com:80/x?y#z":        "http://wardyn.example.com",
		"HTTP://wardyn.example.com:443":             "http://wardyn.example.com:443",
		" https://wardyn.example.com ":              "https://wardyn.example.com",
		"https://[2001:DB8::1]:443/":                "https://[2001:db8::1]",
		"https://[2001:db8::1]:8443":                "https://[2001:db8::1]:8443",
	} {
		if got, ok := urlOrigin(raw); !ok || got != want {
			t.Errorf("urlOrigin(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "   ", "://bad", "https://", "ftp://wardyn.example.com", "wardyn.example.com", "https://%zz"} {
		if got, ok := urlOrigin(raw); ok {
			t.Errorf("urlOrigin(%q) = %q, want refused", raw, got)
		}
	}
}

// TestInboundPerson: only a signed-in person's session counts, never an API token, a delegated request or the
// admin token, which carries no person.
func TestInboundPerson(t *testing.T) {
	person := withHumanIdentity(context.Background(), "sec", "sec@corp.example", oidc.RoleSecurityAdmin, "", nil, false)
	if !inboundPerson(person) {
		t.Error("a session: inboundPerson = false, want true")
	}
	if inboundPerson(withAPITokenID(person, uuid.New())) {
		t.Error("an API token: inboundPerson = true, want false")
	}
	if inboundPerson(audit.WithDelegation(person, types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()})) {
		t.Error("a delegated request: inboundPerson = true, want false")
	}
	if inboundPerson(context.Background()) {
		t.Error("no person (the admin token): inboundPerson = true, want false")
	}
}
