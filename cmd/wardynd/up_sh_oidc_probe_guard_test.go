// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestUpShLocalModeProbeSkipsUnderOIDC is the regression for bug-ops-2: scripts/
// up.sh's post-boot "local-mode no-auth gate" smoke hits the gated /api/v1/me
// endpoint with NO auth token. That only means something in LOCAL_MODE — under
// SSO (WARDYN_OIDC_ISSUER set, per the OIDC-clobber guard earlier in the same
// function) /api/v1/me correctly requires a real session and 401s regardless of
// WARDYN_LOCAL_TRUST_FORWARDER, per TestLocalTrustForwarderAllowsGatewayPeer in
// internal/api/auth_hardening_test.go which exercises LocalTrustForwarder only
// under LocalMode:true. Before the fix that 401 fell into the probe's generic
// "inconclusive, check logs" branch, sending an SSO operator chasing a forwarder
// problem that doesn't exist. up.sh has no shell test harness, so this guard reads
// the script text (matching the repo's existing envdoc/policydoc guard-test
// pattern) and asserts the probe is gated on WARDYN_OIDC_ISSUER before it runs.
func TestUpShLocalModeProbeSkipsUnderOIDC(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "up.sh"))
	if err != nil {
		t.Fatalf("read scripts/up.sh: %v", err)
	}
	src := string(b)

	// Anchor on the actual curl invocation (the full URL), not the surrounding
	// prose — a plain "/api/v1/me" match would also hit the block's own comment
	// explaining the probe, which sits BEFORE the guard this test requires.
	probe := regexp.MustCompile(`http://wardynd:8080/api/v1/me`)
	loc := probe.FindStringIndex(src)
	if loc == nil {
		t.Fatal(`scripts/up.sh no longer probes http://wardynd:8080/api/v1/me — update this guard if the local-mode no-auth smoke moved or was removed`)
	}

	// The nearest preceding OIDC-issuer guard must be an `if` (skip path), not
	// merely present somewhere earlier in the file — else the probe still runs
	// unconditionally under SSO.
	guard := regexp.MustCompile(`if \[ -n "\$\(env_get "\$\{ENV_FILE\}" WARDYN_OIDC_ISSUER\)" \]; then`)
	guardLoc := guard.FindAllStringIndex(src, -1)
	if len(guardLoc) == 0 {
		t.Fatal(`scripts/up.sh: no "WARDYN_OIDC_ISSUER is set" if-guard found — the /api/v1/me probe must be skipped under SSO/OIDC (bug-ops-2)`)
	}
	guarded := false
	for _, g := range guardLoc {
		// The guard must open BEFORE the probe and the probe must be the thing
		// it's guarding, i.e. no unrelated `fi` closes the guard first.
		if g[0] >= loc[0] {
			continue
		}
		between := src[g[1]:loc[0]]
		if !regexp.MustCompile(`(?m)^\s*fi\s*$`).MatchString(between) {
			guarded = true
			break
		}
	}
	if !guarded {
		t.Fatal("scripts/up.sh: the /api/v1/me local-mode no-auth probe runs unguarded under WARDYN_OIDC_ISSUER (SSO) — it must be skipped (bug-ops-2)")
	}
}
