// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := plaintextIssuerRefusal(tc.issuer, tc.internal, tc.posture)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("refused a config that boots: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("booted with a plaintext issuer; want a refusal naming %v", tc.want)
			}
			if !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "plain http://") {
				t.Errorf("refusal = %q, want it to refuse and name the plain http:// issue", err)
			}
			last := -1
			for _, w := range tc.want {
				i := strings.Index(err.Error(), w+" ")
				if i < 0 || i < last {
					t.Fatalf("refusal = %q, want %s named in order", err, w)
				}
				last = i
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

// TestValidateBootPostureWiring proves the wiring: a plaintext issuer fails validateBootPosture,
// the gateway-host warning reaches the process log from it, and a good config logs none.
func TestValidateBootPostureWiring(t *testing.T) {
	run := func(issuer, advertise string) (string, error) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
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
		err := validateBootPosture(f, tlsServed)
		return buf.String(), err
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
