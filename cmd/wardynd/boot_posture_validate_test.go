// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestValidateBootPosture pins the wiring, not the rules: each validator has
// its own table test, so this only proves validateBootPosture still calls the
// UI-sandbox, metrics-listener, base-path and hybrid refusals that run() relies on before migration.
func TestValidateBootPosture(t *testing.T) {
	for _, tc := range []struct {
		name, listen, uiListen, metricsListen, orgURL, basePath, stripCookies string
		memberMode                                                            bool
		wantErr                                                               string // substring; empty = must succeed
	}{
		{name: "nothing set boots", listen: ":8080"},
		{name: "hybrid without member mode refused", listen: ":8080", orgURL: "https://org.example.com", wantErr: "WARDYN_ORG_URL is set but WARDYN_USER_DESKTOP is not"},
		{name: "UI-sandbox on the console address refused", listen: ":8080", uiListen: ":8080", wantErr: "same address as -listen"},
		{name: "metrics listener on the internal listener's address refused", listen: ":8080", metricsListen: ":8443", wantErr: "same address as -internal-listen"},
		{name: "a base path with a trailing slash refused", listen: ":8080", basePath: "/wardyn/", wantErr: "WARDYN_BASE_PATH"},
		{name: "a malformed UI-sandbox cookie policy refused", listen: ":8080", uiListen: ":8081", stripCookies: "allow", wantErr: "WARDYN_UI_SANDBOX_STRIP_COOKIES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sshListen, originTemplate, enrolToken, allowPlaintext := "", "", "", false
			oidcIssuer, oidcInternal, oidcRedirect, controlURL, uiAdvertise := "", "", "", "https://wardynd:8443", ""
			internalListen := ":8443"
			tail, retention := 65536, 30
			rate, seal := 20, "off"
			off, runner, store := false, "none", "pg"
			f := &bootFlags{
				ha: &off, allowMultiInstance: &off, runnerSel: &runner, recordingSel: &store,
				auditSeal:            &seal,
				runOutputTailBytes:   &tail,
				runOutputRetention:   &retention,
				preflightRatePerMin:  &rate,
				basePath:             &tc.basePath,
				oidcIssuer:           &oidcIssuer,
				oidcInternalIss:      &oidcInternal,
				uiAdvertise:          &uiAdvertise,
				oidcRedirectURL:      &oidcRedirect,
				controlURL:           &controlURL,
				listen:               &tc.listen,
				uiListen:             &tc.uiListen,
				sshListen:            &sshListen,
				uiOriginTemplate:     &originTemplate,
				uiStripCookies:       &tc.stripCookies,
				allowPlaintextListen: &allowPlaintext,
				orgURL:               &tc.orgURL,
				orgEnrolToken:        &enrolToken,
				memberMode:           &tc.memberMode,
				internalListen:       &internalListen,
				metricsListen:        &tc.metricsListen,
			}
			err := validateBootPosture(f, tlsPosture{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateRunOutputTailBytes: WARDYN_RUN_OUTPUT_TAIL_BYTES outside 1024 to
// 1048576 is refused at boot, naming the variable.
func TestValidateRunOutputTailBytes(t *testing.T) {
	for _, n := range []int{1024, 65536, 1 << 20} {
		if err := validateRunOutputTailBytes(n); err != nil {
			t.Fatalf("%d refused: %v", n, err)
		}
	}
	for _, n := range []int{512, 0, 2097152} {
		if err := validateRunOutputTailBytes(n); err == nil || !strings.Contains(err.Error(), "WARDYN_RUN_OUTPUT_TAIL_BYTES") {
			t.Fatalf("%d: error %v does not name the variable", n, err)
		}
	}
}

// TestValidateRunOutputRetentionDays: 0 (forever) and a positive window boot; a
// negative one is refused, naming the variable.
func TestValidateRunOutputRetentionDays(t *testing.T) {
	for _, n := range []int{0, 1, 30, 3650} {
		if err := validateRunOutputRetentionDays(n); err != nil {
			t.Fatalf("%d refused: %v", n, err)
		}
	}
	if err := validateRunOutputRetentionDays(-1); err == nil || !strings.Contains(err.Error(), "WARDYN_RUN_OUTPUT_RETENTION_DAYS") {
		t.Fatalf("-1: error %v does not name the variable", err)
	}
}
