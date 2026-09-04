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

	// Anchor on the actual probe invocation, not the surrounding prose — a plain
	// "/api/v1/me" match would also hit the block's own comment explaining the
	// probe, which sits BEFORE the guard this test requires. The URL itself now
	// lives in wardynd_probe (ADV2-01/ADV2-02: one correct way to ask wardynd a
	// question), so the call site is what identifies the smoke.
	probe := regexp.MustCompile(`wardynd_probe "\$\{ENV_FILE\}" /api/v1/me`)
	loc := probe.FindStringIndex(src)
	if loc == nil {
		t.Fatal(`scripts/up.sh no longer probes /api/v1/me via wardynd_probe — update this guard if the local-mode no-auth smoke moved or was removed`)
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

// TestUpShProbeCarriesLoopbackHostAndBearer is the regression for ADV2-01 /
// ADV2-02. scripts/up.sh's post-boot probes run a throwaway curl container on
// the compose network, so the request reaches wardynd from a NON-loopback peer
// (a container on the bridge — the same shape as the docker gateway a real host
// UI/CLI request arrives as). Sent bare, that request is rejected in every
// compose posture and the probes were therefore useless: in local mode the
// DNS-rebinding Host guard 403s the Docker-DNS authority "wardynd:8080"
// (isLoopbackHost, internal/api/http.go), and with local mode off the
// humanOrAdminAuth group 401s an unauthenticated call. So the "local-mode
// no-auth gate REJECTED a non-loopback peer" warning fired on EVERY `make
// setup` — the probe could never return 200 and could never detect the
// forwarder regression it exists to detect — and the post-boot LLM-ready policy
// re-pick behind /api/v1/setup/status was unreachable dead code.
//
// Both are one cause, so there is one fix: wardynd_probe overrides Host with a
// loopback authority (reproducing the real host request's own Host, which
// leaves the PEER gate fully exercised) and carries the admin bearer. This
// guard pins those two headers on the helper; the shell-level behaviour — that
// the probe answers 200 when healthy and still reports 403 when
// WARDYN_LOCAL_TRUST_FORWARDER is off — is pinned by scripts/test-up-probes.sh.
func TestUpShProbeCarriesLoopbackHostAndBearer(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "up.sh"))
	if err != nil {
		t.Fatalf("read scripts/up.sh: %v", err)
	}
	src := string(b)

	fn := regexp.MustCompile(`(?s)\nwardynd_probe\(\) \{.*?\n\}`)
	body := fn.FindString(src)
	if body == "" {
		t.Fatal("scripts/up.sh has no wardynd_probe() — the post-boot probes must ask wardynd through one helper that sends the loopback Host + bearer the gates require (ADV2-01/ADV2-02)")
	}

	if !regexp.MustCompile(`-H "Host: (127\.0\.0\.1|localhost|\[::1\])`).MatchString(body) {
		t.Error(`wardynd_probe does not override Host with a loopback authority — local mode 403s the Docker-DNS authority "wardynd:8080" via the DNS-rebinding guard, so every probe fails on every ` + "`make setup`" + ` (ADV2-01)`)
	}
	if !regexp.MustCompile(`-H "Authorization: Bearer `).MatchString(body) {
		t.Error("wardynd_probe sends no Authorization bearer — /api/v1/me and /api/v1/setup/status sit in the humanOrAdminAuth group and 401 an unauthenticated call whenever local mode is off (ADV2-02)")
	}
	if !regexp.MustCompile(`WARDYN_ADMIN_TOKEN`).MatchString(body) {
		t.Error("wardynd_probe does not read WARDYN_ADMIN_TOKEN from the env file — it would send a token wardynd never booted with")
	}

	// One way to ask: any OTHER in-network curl invocation is a second, bare
	// request shape that the gates reject exactly as before the fix.
	if n := len(regexp.MustCompile(`curlimages/curl`).FindAllString(src, -1)); n != 1 {
		t.Errorf("scripts/up.sh has %d curlimages/curl invocations, want exactly 1 (inside wardynd_probe) — a bare in-network probe is answered 403/401 in every compose posture (ADV2-01/ADV2-02)", n)
	}
}
