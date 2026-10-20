// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// healthzServer answers /healthz with cliSignIn ("" sends no field, "404" no route) and counts the reads.
func healthzServer(t *testing.T, cliSignIn string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("/healthz was read with a credential")
		}
		switch cliSignIn {
		case "404":
			http.NotFound(w, r)
		case "":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			_, _ = fmt.Fprintf(w, `{"status":"ok","cli_sign_in":%s}`, cliSignIn)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reads
}

// TestUnauthorizedHint is DESIGN-ATTACH §11.7's table: each 401 reason against each cli_sign_in answer,
// byte for byte.
func TestUnauthorizedHint(t *testing.T) {
	const (
		signInCLI    = "run `wardyn login`"
		signInBefore = "this organisation signs people in with single sign-on and has no admin token. Set WARDYN_TOKEN to a personal API token (--token and WARDYN_ADMIN_TOKEN take one too; --token shows it in the process list): a Wardyn admin can invite you to create one, and you claim it under Your account in the console."
		unknown      = "the token in WARDYN_TOKEN is not one this organisation knows (unknown, revoked or expired)"
		unknownCLI   = "the token in WARDYN_TOKEN is not one this organisation knows (unknown, revoked or expired); it takes precedence over `wardyn login`; unset it to use your sign-in"
		today        = "authenticate with --token or WARDYN_ADMIN_TOKEN/WARDYN_TOKEN (the compose quick-start's token is in deploy/compose/README.md)"
	)
	for _, tc := range []struct {
		reason, cliSignIn, want string
		reads                   int32
	}{
		{"sign_in_required", "true", signInCLI, 1},
		{"sign_in_required", "false", signInBefore, 1},
		{"sign_in_required", "", signInBefore, 1},
		{"sign_in_required", "404", signInBefore, 1},
		{"admin_token_not_configured", "true", unknownCLI, 1},
		{"admin_token_not_configured", "false", unknown, 1},
		{"admin_token_not_configured", "", unknown, 1},
		{"admin_token_not_configured", "404", unknown, 1},
		{"identity_deactivated", "true", today, 0},
		{"identity_deactivated", "false", today, 0},
		{"", "true", today, 0},
		{"", "false", today, 0},
	} {
		t.Run(tc.reason+"/cli_sign_in="+tc.cliSignIn, func(t *testing.T) {
			srv, reads := healthzServer(t, tc.cliSignIn)
			old := authHintFor
			t.Cleanup(func() { authHintFor = old })
			authHintFor.url, authHintFor.tokenFrom = srv.URL, "WARDYN_TOKEN"
			got := dialHint(&sdk.APIError{Status: http.StatusUnauthorized, Reason: tc.reason})
			if got != tc.want {
				t.Errorf("hint = %q\nwant %q", got, tc.want)
			}
			// /healthz is read once, and only after the two reasons whose remedy it chooses.
			if n := reads.Load(); n != tc.reads {
				t.Errorf("/healthz reads = %d, want %d", n, tc.reads)
			}
			if tc.reason == "sign_in_required" && (strings.Contains(got, "compose") || strings.Contains(got, "WARDYN_ADMIN_TOKEN=")) {
				t.Errorf("sign_in_required hint %q names the compose stack or sets an admin token", got)
			}
		})
	}
}

// TestUnauthorizedHintNamesTheTokenSource: the line names the place the CLI took the bearer from.
func TestUnauthorizedHintNamesTheTokenSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","reason":"admin_token_not_configured"}`))
	}))
	defer srv.Close()
	old := authHintFor
	t.Cleanup(func() { authHintFor = old })
	for _, tc := range []struct {
		admin, user, flag, want string
	}{
		{"", "u", "", "WARDYN_TOKEN"},
		{"a", "u", "", "WARDYN_ADMIN_TOKEN"},
		{"a", "", "f", "--token"},
	} {
		t.Setenv("WARDYN_ADMIN_TOKEN", tc.admin)
		t.Setenv("WARDYN_TOKEN", tc.user)
		args := []string{"--url", srv.URL, "run", "list"}
		if tc.flag != "" {
			args = append([]string{"--token", tc.flag}, args...)
		}
		err := execCmd(t, args...)
		want := "the token in " + tc.want + " is not one this organisation knows (unknown, revoked or expired)"
		if got := dialHint(err); got != want {
			t.Errorf("%+v: hint = %q, want %q (err %v)", tc, got, want, err)
		}
	}
}
