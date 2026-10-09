// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

var tlsServed = tlsPosture{tlsEnabled: true, secureCookies: true}

// TestBootPosturePlaintextIssuerRefusal pins #156 as a refusal: a non-loopback http:// issuer is
// refused once the console has a TLS posture, the error names each offending variable, and
// https, loopback and the plaintext Compose demo's bundled Dex still boot.
func TestBootPosturePlaintextIssuerRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, internal string
		optOut                 string
		posture                tlsPosture
		want                   []string // env names that must be named, in order; none = boots
	}{
		{name: "https issuer boots", issuer: "https://login.example.com/v2.0", posture: tlsServed},
		{name: "loopback http public issuer boots", issuer: "http://localhost:5556", posture: tlsServed},
		{name: "loopback ip boots", issuer: "http://127.0.0.1:5556", internal: "http://[::1]:5556", posture: tlsServed},
		{name: "no issuer boots", posture: tlsServed},
		{name: "compose demo internal dex boots", issuer: "http://localhost:5556", internal: "http://dex:5556"},
		{name: "oidc off: compose default internal issuer boots", internal: "http://dex:5556", posture: tlsServed},
		{name: "plaintext public issuer is refused", issuer: "http://idp.example.com/", posture: tlsServed, want: []string{"WARDYN_OIDC_ISSUER"}},
		{name: "plaintext internal issuer is refused", issuer: "https://idp.example.com/", internal: "http://dex:5556", posture: tlsServed, want: []string{"WARDYN_OIDC_INTERNAL_ISSUER"}},
		{name: "both are named", issuer: "http://idp.example.com", internal: "http://10.0.0.7:5556", posture: tlsServed,
			want: []string{"WARDYN_OIDC_ISSUER", "WARDYN_OIDC_INTERNAL_ISSUER"}},
		{name: "scheme is case-folded", issuer: "HTTP://idp.example.com", posture: tlsServed, want: []string{"WARDYN_OIDC_ISSUER"}},
		{name: "mesh opt-out lets the internal issuer boot", issuer: "https://idp.example.com/", internal: "http://dex:5556", optOut: "mesh", posture: tlsServed},
		{name: "mesh opt-out never covers the public issuer", issuer: "http://idp.example.com", internal: "http://dex:5556", optOut: "mesh", posture: tlsServed,
			want: []string{"WARDYN_OIDC_ISSUER"}},
		{name: "unknown opt-out value is refused", issuer: "https://idp.example.com/", internal: "http://dex:5556", optOut: "yes", posture: tlsServed,
			want: []string{"WARDYN_OIDC_INTERNAL_ISSUER_PLAINTEXT"}},
		{name: "unknown opt-out value is refused even with nothing plaintext", issuer: "https://idp.example.com/", optOut: "true",
			want: []string{"WARDYN_OIDC_INTERNAL_ISSUER_PLAINTEXT"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := plaintextIssuerRefusal(tc.issuer, tc.internal, tc.optOut, tc.posture)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("refused a config that boots: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("booted with a plaintext issuer; want a refusal naming %v", tc.want)
			}
			if !strings.Contains(err.Error(), "refusing to start") {
				t.Errorf("refusal = %q, want it to refuse", err)
			}
			if tc.optOut != "" && tc.optOut != "mesh" && !strings.Contains(err.Error(), `"mesh"`) {
				t.Errorf("refusal = %q, want it to name the accepted value", err)
			}
			last := -1
			for _, w := range tc.want {
				i := strings.Index(err.Error(), w)
				if i < 0 || i < last {
					t.Fatalf("refusal = %q, want %s named in order", err, w)
				}
				last = i
			}
		})
	}
}

// TestPlaintextInternalMeshWarning: the opt-out is a posture warning, only while it is carrying a plain
// http:// internal issuer under a TLS posture.
func TestPlaintextInternalMeshWarning(t *testing.T) {
	const pub = "https://idp.example.com/"
	w := plaintextInternalMeshWarning(pub, "http://dex:5556", "mesh", tlsServed)
	for _, want := range []string{"WARDYN_OIDC_INTERNAL_ISSUER", "http://dex:5556", "encrypted by a service mesh"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q lacks %q", w, want)
		}
	}
	for name, got := range map[string]string{
		"no opt-out":       plaintextInternalMeshWarning(pub, "http://dex:5556", "", tlsServed),
		"https internal":   plaintextInternalMeshWarning(pub, "https://dex:5556", "mesh", tlsServed),
		"loopback":         plaintextInternalMeshWarning(pub, "http://localhost:5556", "mesh", tlsServed),
		"no TLS posture":   plaintextInternalMeshWarning(pub, "http://dex:5556", "mesh", tlsPosture{}),
		"no public issuer": plaintextInternalMeshWarning("", "http://dex:5556", "mesh", tlsServed),
	} {
		if got != "" {
			t.Errorf("%s: warned: %q", name, got)
		}
	}
}

// TestBootPostureUIGatewaySharesConsoleHost pins #1269: under secure cookies a
// path-mode gateway advertised on the console's own hostname warns and names
// the remedy; a hostname of its own, host mode, the gateway off, or plain-HTTP
// cookies stay quiet. Ports are ignored on purpose — cookies ignore them.
func TestBootPostureUIGatewaySharesConsoleHost(t *testing.T) {
	const redirect = "https://wardyn.example.com/auth/callback"
	for _, tc := range []struct {
		name, uiListen, advertise, template, redirect string
		posture                                       tlsPosture
		warns                                         bool
	}{
		{name: "same host different port warns", uiListen: ":8081", advertise: "https://wardyn.example.com:8081", redirect: redirect, posture: tlsServed, warns: true},
		{name: "same host folded warns", uiListen: ":8081", advertise: "https://WARDYN.example.com/ui", redirect: redirect, posture: tlsServed, warns: true},
		{name: "own hostname is quiet", uiListen: ":8081", advertise: "https://wardyn-ui.example.com", redirect: redirect, posture: tlsServed},
		{name: "gateway off is quiet", advertise: "https://wardyn.example.com:8081", redirect: redirect, posture: tlsServed},
		{name: "host mode is quiet", uiListen: ":8081", advertise: "https://wardyn.example.com:8081", template: "https://run-{run}.ui.example.com", redirect: redirect, posture: tlsServed},
		{name: "plain-http cookies are quiet", uiListen: ":8081", advertise: "http://wardyn.example.com:8081", redirect: "http://wardyn.example.com/auth/callback"},
		{name: "no advertise is quiet", uiListen: ":8081", redirect: redirect, posture: tlsServed},
		{name: "no redirect url is quiet", uiListen: ":8081", advertise: "https://wardyn.example.com", posture: tlsServed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := uiGatewaySharesConsoleHostWarning(tc.uiListen, tc.advertise, tc.template, tc.redirect, tc.posture)
			if !tc.warns {
				if w != "" {
					t.Fatalf("warned on a good config: %q", w)
				}
				return
			}
			for _, part := range []string{"WARDYN_UI_SANDBOX_ADVERTISE", "wardyn.example.com", "__Host-wardyn_session", "its own hostname"} {
				if !strings.Contains(strings.ToLower(w), strings.ToLower(part)) {
					t.Errorf("warning %q does not mention %q", w, part)
				}
			}
		})
	}
}

// postureFlags is a bootFlags that passes validateBootPosture under tlsServed; set puts the case's own
// values on top.
func postureFlags(issuer, advertise string, set func(*bootFlags)) *bootFlags {
	listen, ui, ssh, tmpl, strip, base := ":8080", ":8081", "", "", "", ""
	redirect, control, internal, org, tok := "https://wardyn.example.com/auth/callback", "https://wardynd:8443", "", "", ""
	allow, member, tail, rate, retention, seal := false, false, 65536, 20, 30, "off"
	off, runner, store := false, "none", "pg"
	var sessionTTL time.Duration
	f := &bootFlags{
		ha: &off, allowMultiInstance: &off, runnerSel: &runner, recordingSel: &store,
		auditSeal:           &seal,
		runOutputTailBytes:  &tail,
		runOutputRetention:  &retention,
		preflightRatePerMin: &rate,
		basePath:            &base, oidcIssuer: &issuer, oidcInternalIss: &internal, oidcRedirectURL: &redirect, controlURL: &control,
		listen: &listen, uiListen: &ui, uiAdvertise: &advertise, sshListen: &ssh, sshProxyCommand: &tmpl, uiOriginTemplate: &tmpl, uiStripCookies: &strip,
		allowPlaintextListen: &allow, orgURL: &org, orgEnrolToken: &tok, memberMode: &member,
		internalListen: &ssh, metricsListen: &ssh,
		oidcSessionTTL: &sessionTTL,
	}
	if set != nil {
		set(f)
	}
	return f
}

// TestValidateBootPostureWiring proves the wiring: a plaintext issuer fails validateBootPosture,
// the gateway-host warning reaches the process log from it, and a good config logs none.
func TestValidateBootPostureWiring(t *testing.T) {
	run := func(issuer, advertise string) (string, error) {
		logs := captureSlog(t)
		err := validateBootPosture(postureFlags(issuer, advertise, nil), tlsServed)
		return logs.String(), err
	}
	if _, err := run("http://idp.example.com", "https://wardyn-ui.example.com"); err == nil || !strings.Contains(err.Error(), "WARDYN_OIDC_ISSUER") {
		t.Errorf("plaintext issuer: err = %v, want a refusal naming WARDYN_OIDC_ISSUER", err)
	}
	if out, err := run("https://idp.example.com", "https://wardyn.example.com:8081"); err != nil || !strings.Contains(out, "WARDYN_UI_SANDBOX_ADVERTISE") {
		t.Errorf("bad gateway host: err = %v, log %q, want the warning and a boot", err, out)
	}
	if out, err := run("https://idp.example.com", "https://wardyn-ui.example.com"); err != nil || strings.Contains(out, "level=WARN") {
		t.Errorf("good config: err = %v, log %q, want a clean boot", err, out)
	}
}

// TestValidateBootPostureMeshOptOut: the opt-out reaches boot. A plain internal issuer is refused
// without it, boots with it and warns, and a value other than "mesh" is refused.
func TestValidateBootPostureMeshOptOut(t *testing.T) {
	boot := func(internal, optOut string) (string, error) {
		logs := captureSlog(t)
		f := postureFlags("https://idp.example.com", "https://wardyn-ui.example.com", func(f *bootFlags) {
			f.oidcInternalIss, f.oidcInternalIssPlain = &internal, &optOut
		})
		err := validateBootPosture(f, tlsServed)
		return logs.String(), err
	}
	if _, err := boot("http://dex:5556", ""); err == nil || !strings.Contains(err.Error(), "WARDYN_OIDC_INTERNAL_ISSUER") {
		t.Errorf("no opt-out: err = %v, want the internal issuer refused", err)
	}
	out, err := boot("http://dex:5556", "mesh")
	if err != nil || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "encrypted by a service mesh") {
		t.Errorf("mesh: err = %v, log %q, want a boot with the posture warning", err, out)
	}
	if _, err := boot("http://dex:5556", "true"); err == nil || !strings.Contains(err.Error(), `"mesh"`) {
		t.Errorf("opt-out true: err = %v, want a refusal naming the accepted value", err)
	}
}

// TestValidateBootPostureRefusesMemberBeforeMigrations: validateBootPosture runs before the database is
// migrated, so a leftover "member" role is refused here, not after a forward-only migration.
func TestValidateBootPostureRefusesMemberBeforeMigrations(t *testing.T) {
	for name, tc := range map[string]struct{ roleMap, defaultRole, want string }{
		"role map":     {roleMap: "eng=member", want: "parse WARDYN_OIDC_ROLE_MAP"},
		"default role": {defaultRole: "member", want: "WARDYN_OIDC_DEFAULT_ROLE"},
	} {
		t.Run(name, func(t *testing.T) {
			f := postureFlags("https://idp.example.com", "https://wardyn-ui.example.com", func(f *bootFlags) {
				f.oidcRoleMap, f.oidcDefaultRole = &tc.roleMap, &tc.defaultRole
			})
			err := validateBootPosture(f, tlsServed)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"member"`) {
				t.Fatalf("err = %v, want a refusal naming %s and the member value", err, tc.want)
			}
		})
	}
	// No issuer: the settings are inert, as at the later parse.
	noIssuer, member := "", "member"
	f := postureFlags(noIssuer, "https://wardyn-ui.example.com", func(f *bootFlags) { f.oidcRoleMap = &member })
	if err := validateBootPosture(f, tlsServed); err != nil {
		t.Errorf("a role map with no issuer was refused: %v", err)
	}
}
