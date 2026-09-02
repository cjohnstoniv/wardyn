// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F10-install-trust-path guards. install.sh has no harness of its own, so these
// read the script text — the pattern up_sh_oidc_probe_guard_test.go established;
// repoRoot is from envdoc_guard_test.go. The end-to-end half (install.sh actually
// executed against a stub docker/curl/chmod) is scripts/test-install-sh-trust.sh,
// which `make test-scripts` runs.
//
// Run (no Postgres, no build tags):
//
//	nice -n 10 GOMAXPROCS=8 go test -p 4 ./cmd/wardynd -run 'TestInstallSh|TestComposeDefault' -count=1
//
// H1/H2 (the admin token) and H6 (the .env mode) are FIXED — these now pin the
// fix. H3 (the compose fetch has no digest) is an accepted risk for 0.7,
// published in docs/VERIFY.md and threatmodel/THREAT-MODEL.md §5, so
// TestInstallSh_ComposeFetchIsVerified SKIPS unless F10_EXPECT_COMPOSE_INTEGRITY=1.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readInstallSh(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	return string(b)
}

// The admin token was `docker run … -gen-age-key | sha256sum | cut -c1-48`.
// Two independent failures both yielded a token that is not a secret, and
// neither is caught by `set -eu` because POSIX sh has no pipefail:
//   - `sha256sum` absent (macOS ships shasum, not GNU coreutils; install_cli's
//     SHA256SUMS check already knew that) -> TOKEN="" -> the compose file's
//     `${WARDYN_ADMIN_TOKEN:-demo-admin-token}` substitutes the published token;
//   - the second `docker run` failing -> sha256("") -> a public constant.
//
// Both are closed by install.sh's mint_admin_token: sha256_hex carries the
// shasum fallback for BOTH hashing sites, and mint_age_key checks the mint on
// its own line before anything hashes it.
//
// The `shasum -a 256` requirement below is deliberately literal. sha256_hex's
// else-branch is the ONLY line that runs on a mac, and until T2 of
// scripts/test-install-sh-trust.sh was given a real mac-shaped PATH nothing
// executed it — the branch could have been deleted and every gate stayed green.
// A derivation that needs no hashing binary at all (a `-gen-admin-token` flag,
// say) is still welcome; it just has to update this guard on the way in, which
// is the point of pinning it.
func TestInstallSh_AdminTokenDerivationFailsClosed(t *testing.T) {
	src := readInstallSh(t)
	tokLine := regexp.MustCompile(`(?m)^\s*TOKEN=\$\(.*$`).FindString(src)
	if tokLine == "" {
		t.Fatal("install.sh no longer derives TOKEN=$(...) — update this guard to the new derivation")
	}
	if strings.Contains(tokLine, "sha256sum") && !strings.Contains(tokLine, "shasum") {
		t.Errorf("install.sh's admin-token line depends on `sha256sum` with no `shasum` fallback (sha256_hex, which install_cli's SHA256SUMS check uses, has one): on macOS TOKEN is empty and compose falls through to demo-admin-token\n  %s", strings.TrimSpace(tokLine))
	}
	if !strings.Contains(src, "shasum -a 256") {
		t.Error("install.sh has no `shasum -a 256` fallback anywhere: macOS ships no GNU coreutils, so every hashing site — the admin-token mint and install_cli's SHA256SUMS check — silently produces nothing there")
	}
	// A non-empty guard must sit on TOKEN the way install.sh guards KEY.
	guard := regexp.MustCompile(`\[\s+-n\s+"\$\{?TOKEN\}?"\s+\]\s*\|\|\s*die`)
	if !guard.MatchString(src) {
		t.Errorf("install.sh has no `[ -n \"$TOKEN\" ] || die` guard: a failed second `docker run … -gen-age-key` hashes an empty stdin and installs sha256(\"\")[0:48] as the admin token")
	}
}

// .env carries WARDYN_AGE_KEY (the secret-store master key) and the admin
// token. `cat > .env` and the upgrade path's env_set `> .env.tmp && mv` both
// created the file under the caller's umask and only chmod 600 afterwards; on a
// multi-user host that window is enough to read both. install.sh now sets
// `umask 077` around the whole .env block, restoring OLD_UMASK after it so
// install_cli's PATH directories keep their normal modes.
//
// Both `chmod 600 .env` calls are GONE, and their absence is the point: under
// that umask neither could do anything but re-assert the mode the file already
// had, while their presence made T4 look satisfied by a chmod. An .env an older
// install created 0644 is still fixed — the upgrade branch rewrites the file
// through `.env.tmp` + `mv`, and that tmp is born 0600. T4 and T5 of
// scripts/test-install-sh-trust.sh assert the resulting mode with a
// record-only chmod stub in place.
func TestInstallSh_EnvWrittenUnderRestrictiveUmask(t *testing.T) {
	src := readInstallSh(t)
	firstWrite := strings.Index(src, "cat > .env")
	if firstWrite < 0 {
		t.Fatal("install.sh no longer writes .env via `cat > .env` — update this guard")
	}
	umask := strings.Index(src, "umask 077")
	if umask < 0 || umask > firstWrite {
		t.Errorf("install.sh writes .env (offset %d) with no preceding `umask 077` (found at %d): the age key + admin token would be world-readable until the later chmod 600", firstWrite, umask)
	}
}

// The compose definition — the file that decides which image runs, which
// ports publish on which interface, whether WARDYN_LOCAL_MODE is on, and what
// gets bind-mounted (the docker socket) — is fetched from a MUTABLE tag ref
// over raw.githubusercontent and is not among the cosign-signed release assets
// (release.yml's checksums + cosign-sign-blob steps). For 0.7 that is a
// PUBLISHED accepted risk — docs/VERIFY.md "6. What the one-line installer
// checks — and what it leaves to you" and threatmodel/THREAT-MODEL.md §5
// residual 32 — so this skips. Once a digest exists it pins that install.sh
// consults it and dies on a mismatch.
func TestInstallSh_ComposeFetchIsVerified(t *testing.T) {
	if os.Getenv("F10_EXPECT_COMPOSE_INTEGRITY") != "1" {
		t.Skip("accepted risk until deploy/compose/docker-compose.yaml is covered by SHA256SUMS or a pinned digest (docs/VERIFY.md; THREAT-MODEL §5 residual 32); set F10_EXPECT_COMPOSE_INTEGRITY=1 to enforce")
	}
	src := readInstallSh(t)
	fetch := strings.Index(src, "deploy/compose/docker-compose.yaml")
	if fetch < 0 {
		t.Fatal("install.sh no longer fetches deploy/compose/docker-compose.yaml — update this guard")
	}
	after := src[fetch:]
	// Go's regexp caps a repeat count at 1000, so the window is cut in code:
	// the checksum must sit within 1200 bytes of the compose fetch, and its
	// fail-closed die within 400 bytes of the checksum.
	if len(after) > 1600 {
		after = after[:1600]
	}
	if !regexp.MustCompile(`docker-compose\.yaml[\s\S]*?(SHA256SUMS|sha256sum|shasum)[\s\S]{0,400}die "`).MatchString(after) {
		t.Errorf("install.sh fetches the compose file and never checks it against SHA256SUMS/a digest with a fail-closed die")
	}
}

// PINS the fall-through that turns an empty install-time token into a
// published credential: the compose file's `${WARDYN_ADMIN_TOKEN:-demo-admin-token}`
// (the wardynd service's WARDYN_ADMIN_TOKEN row) substitutes for BOTH unset and
// empty (Compose `:-` semantics), and resolveLocalMode boots on that token when
// the listen is the unspecified ":8080" the container uses (see resolveLocalMode
// in boot_flags.go and its demo-token row in config_test.go). If either half
// changes, this test says so and the F10 trace doc needs a rewrite.
func TestComposeDefault_EmptyAdminTokenBecomesDemoToken(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "compose", "docker-compose.yaml"))
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	if !strings.Contains(string(b), `WARDYN_ADMIN_TOKEN: "${WARDYN_ADMIN_TOKEN:-demo-admin-token}"`) {
		t.Fatalf("docker-compose.yaml no longer defaults WARDYN_ADMIN_TOKEN via `:-demo-admin-token` — the H1/H2 fall-through in F10-install-trust-path.md changed; re-trace")
	}
	listen := ":8080"
	tok := demoAdminToken
	f := &bootFlags{
		listen:        &listen,
		adminToken:    &tok,
		localMode:     new(bool),
		localOperator: new(string),
		oidcIssuer:    new(string),
		localTrustFwd: new(bool),
	}
	lm, err := resolveLocalMode(f)
	if err != nil {
		t.Fatalf("resolveLocalMode refused the compose-shaped demo-token boot (this is the SAFE direction; update F10-install-trust-path.md H8): %v", err)
	}
	if lm.enabled {
		t.Fatalf("demo token + unspecified bind resolved to LocalMode=true; expected token mode")
	}
}
