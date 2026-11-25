// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
)

// P10 (pg): a console_issuers row makes the issuer known, whatever its spelling under NormIssuerURL.
func TestConsoleIssuerKnown_PG(t *testing.T) {
	pool := portalBootPool(t, "ckn")
	const issuer = "https://login.console.example/tenant"
	k := consoleIssuerKnown(pool)
	if got, err := k(context.Background(), issuer); got || err != nil {
		t.Fatalf("before the issuer was recorded: known %v, %v; want false, nil", got, err)
	}
	if err := recordConsoleIssuer(context.Background(), pool, issuer, "r1"); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{issuer, "HTTPS://Login.Console.example:443/tenant/"} {
		if got, err := k(context.Background(), spelling); !got || err != nil {
			t.Errorf("after recording: %q known %v, %v; want true", spelling, got, err)
		}
	}
	if got, _ := k(context.Background(), "https://other.example"); got {
		t.Error("an issuer that was never recorded is known")
	}
}

// P9 (pg): through the real boot path, a known issuer whose provider does not answer boots with a non-nil, pending
// authenticator, and the posture refusals that read "SSO is configured" still fire.
func TestBuildOptionalFeatures_PendingKeepsPostureRefusals(t *testing.T) {
	pool := portalBootPool(t, "bpk")
	p := newPendingIdP(t)
	if err := recordConsoleIssuer(context.Background(), pool, p.srv.URL, "r1"); err != nil {
		t.Fatal(err)
	}
	p.rt.set(pendingDiscoveryPath, 503)
	newStore := func() *memSecretStore {
		return &memSecretStore{vals: map[string][]byte{secretSessionKey: []byte("01234567890123456789012345678901")}}
	}
	boot := func(edit func(*bootFlags)) (optionalFeatures, error) {
		f := defaultRoleBootFlags(p.srv.URL, "user")
		if edit != nil {
			edit(f)
		}
		rootCtx, cancel := context.WithCancel(p.ctx)
		t.Cleanup(cancel)
		return buildOptionalFeatures(rootCtx, p.ctx, f, pool, newStore(), unlocked(newStore()), false)
	}
	of, err := boot(nil)
	if err != nil || of.authn == nil || !of.authn.Pending() {
		t.Fatalf("a known issuer whose provider does not answer = authn %v, error %v; want a pending authenticator", of.authn, err)
	}
	if _, err := boot(func(f *bootFlags) { *f.oidcOperatorEmails = "" }); err == nil {
		t.Error("an empty operator list booted pending: the operator-list refusal stopped firing")
	}
	_, err = boot(func(f *bootFlags) { *f.ssoOnly, *f.adminToken = true, "an-admin-token" })
	if err == nil || !strings.Contains(err.Error(), "WARDYN_SSO_ONLY") {
		t.Errorf("SSO-only beside an admin token booted pending: %v", err)
	}
}
