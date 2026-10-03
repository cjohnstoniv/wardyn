// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

var tlsServed = tlsPosture{tlsEnabled: true, secureCookies: true}

// TestBootPosturePlaintextIssuerWarnings pins #156: a non-loopback http:// issuer warns
// once the console has a TLS posture, names the variable, and stays silent for
// https, for loopback, and for the plaintext Compose demo's bundled Dex.
func TestBootPosturePlaintextIssuerWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, internal string
		posture                tlsPosture
		want                   []string // env names that must be named, in order
	}{
		{name: "https issuer is silent", issuer: "https://login.example.com/v2.0", posture: tlsServed},
		{name: "loopback http public issuer is silent", issuer: "http://localhost:5556", posture: tlsServed},
		{name: "loopback ip is silent", issuer: "http://127.0.0.1:5556", internal: "http://[::1]:5556", posture: tlsServed},
		{name: "no issuer is silent", posture: tlsServed},
		{name: "compose demo internal dex is silent", issuer: "http://localhost:5556", internal: "http://dex:5556"},
		{name: "oidc off: compose default internal issuer is silent", internal: "http://dex:5556", posture: tlsServed},
		{name: "plaintext public issuer warns", issuer: "http://idp.example.com/", posture: tlsServed, want: []string{"WARDYN_OIDC_ISSUER"}},
		{name: "plaintext internal issuer warns", issuer: "https://idp.example.com/", internal: "http://dex:5556", posture: tlsServed, want: []string{"WARDYN_OIDC_INTERNAL_ISSUER"}},
		{name: "both warn", issuer: "http://idp.example.com", internal: "http://10.0.0.7:5556", posture: tlsServed,
			want: []string{"WARDYN_OIDC_ISSUER", "WARDYN_OIDC_INTERNAL_ISSUER"}},
		{name: "scheme is case-folded", issuer: "HTTP://idp.example.com", posture: tlsServed, want: []string{"WARDYN_OIDC_ISSUER"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := plaintextIssuerWarnings(tc.issuer, tc.internal, tc.posture)
			if len(got) != len(tc.want) {
				t.Fatalf("warnings = %q, want one per %v", got, tc.want)
			}
			for i, w := range got {
				if !strings.Contains(w, tc.want[i]) || !strings.Contains(w, "plain http://") {
					t.Errorf("warning %d = %q, want it to name %s and the plain http:// issue", i, w, tc.want[i])
				}
			}
		})
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

// TestValidateBootPostureLogsTheWarnings proves the wiring: both warnings
// reach the process log from validateBootPosture, and a good config logs none.
func TestValidateBootPostureLogsTheWarnings(t *testing.T) {
	run := func(issuer, advertise string) string {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		listen, ui, ssh, tmpl, strip, base := ":8080", ":8081", "", "", "", ""
		redirect, control, internal, org, tok := "https://wardyn.example.com/auth/callback", "https://wardynd:8443", "", "", ""
		allow, member, rate := false, false, 20
		f := &bootFlags{
			preflightRatePerMin: &rate,
			basePath:            &base, oidcIssuer: &issuer, oidcInternalIss: &internal, oidcRedirectURL: &redirect, controlURL: &control,
			listen: &listen, uiListen: &ui, uiAdvertise: &advertise, sshListen: &ssh, uiOriginTemplate: &tmpl, uiStripCookies: &strip,
			allowPlaintextListen: &allow, orgURL: &org, orgEnrolToken: &tok, memberMode: &member,
		}
		if err := validateBootPosture(f, tlsServed); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := run("http://idp.example.com", "https://wardyn.example.com:8081"); !strings.Contains(out, "WARDYN_OIDC_ISSUER") || !strings.Contains(out, "WARDYN_UI_SANDBOX_ADVERTISE") {
		t.Errorf("bad config logged %q, want both warnings", out)
	}
	if out := run("https://idp.example.com", "https://wardyn-ui.example.com"); strings.Contains(out, "level=WARN") {
		t.Errorf("good config logged %q, want no warning", out)
	}
}
