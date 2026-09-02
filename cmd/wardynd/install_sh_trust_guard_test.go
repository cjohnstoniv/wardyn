// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F10-install-trust-path probe. Intended destination: cmd/wardynd/install_sh_trust_guard_test.go
// (beside up_sh_oidc_probe_guard_test.go, which established the "guard reads the
// script text" pattern for shell that has no harness of its own; repoRoot(t) is
// envdoc_guard_test.go:175).
//
// Run (no Postgres, no build tags; the coordinator runs probes serially):
//
//	cp local/review-0.7/deep/F10-install-trust-path/install_sh_trust_guard_test.go cmd/wardynd/
//	nice -n 10 GOMAXPROCS=8 go test -p 4 ./cmd/wardynd -run 'TestInstallSh|TestComposeDefault' -count=1
//	rm cmd/wardynd/install_sh_trust_guard_test.go
//
// Expected TODAY: TestInstallSh_AdminTokenDerivationFailsClosed FAILS (H1/H2),
// TestInstallSh_EnvWrittenUnderRestrictiveUmask FAILS (H6),
// TestInstallSh_ComposeFetchIsVerified SKIPS unless F10_EXPECT_COMPOSE_INTEGRITY=1 (H3),
// TestComposeDefault_EmptyAdminTokenBecomesDemoToken PASSES (it PINS the fall-through
// that turns H1/H2 into a published credential — a change to that default breaks it
// on purpose so the doc gets updated).

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

// The admin token is `docker run … -gen-age-key | sha256sum | cut -c1-48`
// (install.sh:89). Two independent failures both yield a token that is not a
// secret, and neither is caught by `set -eu` because POSIX sh has no pipefail:
//   - `sha256sum` absent (macOS ships shasum, not GNU coreutils; install.sh:188
//     already knows this for the CLI checksum) -> TOKEN="" -> the compose file's
//     `${WARDYN_ADMIN_TOKEN:-demo-admin-token}` substitutes the published token;
//   - the second `docker run` failing -> sha256("") -> a public constant.
//
// Either a shasum fallback + a non-empty guard, or a derivation that does not
// depend on a hashing binary at all (e.g. a second `-gen-age-key` used verbatim
// after stripping its prefix, or a `-gen-admin-token` flag) closes both.
func TestInstallSh_AdminTokenDerivationFailsClosed(t *testing.T) {
	src := readInstallSh(t)
	tokLine := regexp.MustCompile(`(?m)^\s*TOKEN=\$\(.*$`).FindString(src)
	if tokLine == "" {
		t.Fatal("install.sh no longer derives TOKEN=$(...) — update this guard to the new derivation")
	}
	if strings.Contains(tokLine, "sha256sum") && !strings.Contains(tokLine, "shasum") {
		t.Errorf("install.sh's admin-token line depends on `sha256sum` with no `shasum` fallback (the CLI checksum at install.sh:188-189 has one): on macOS TOKEN is empty and compose falls through to demo-admin-token\n  %s", strings.TrimSpace(tokLine))
	}
	// A non-empty guard must sit on TOKEN the way install.sh:88 guards KEY.
	guard := regexp.MustCompile(`\[\s+-n\s+"\$\{?TOKEN\}?"\s+\]\s*\|\|\s*die`)
	if !guard.MatchString(src) {
		t.Errorf("install.sh has no `[ -n \"$TOKEN\" ] || die` guard: a failed second `docker run … -gen-age-key` hashes an empty stdin and installs sha256(\"\")[0:48] as the admin token")
	}
}

// .env carries WARDYN_AGE_KEY (the secret-store master key) and the admin
// token. `cat > .env` (install.sh:91) and the upgrade path's `> .env.tmp && mv`
// (install.sh:130-131,138) both create the file under the caller's umask and
// only chmod 600 afterwards (install.sh:115,146). A `umask 077` before the
// first write removes the window on multi-user hosts.
func TestInstallSh_EnvWrittenUnderRestrictiveUmask(t *testing.T) {
	src := readInstallSh(t)
	firstWrite := strings.Index(src, "cat > .env")
	if firstWrite < 0 {
		t.Fatal("install.sh no longer writes .env via `cat > .env` — update this guard")
	}
	umask := strings.Index(src, "umask 077")
	if umask < 0 || umask > firstWrite {
		t.Errorf("install.sh writes .env (offset %d) with no preceding `umask 077` (found at %d): the age key + admin token are world-readable until the later chmod 600", firstWrite, umask)
	}
}

// The compose definition — the file that decides which image runs, which
// ports publish on which interface, whether WARDYN_LOCAL_MODE is on, and what
// gets bind-mounted (the docker socket) — is fetched from a MUTABLE tag ref
// over raw.githubusercontent (install.sh:80) and is not among the cosign-signed
// release assets (release.yml:444-452,477-478). Until a digest exists this is
// an ACCEPTED RISK (see ACCEPTED-RISK-compose-fetch.md); once one does, this
// guard pins that install.sh consults it and dies on a mismatch.
func TestInstallSh_ComposeFetchIsVerified(t *testing.T) {
	if os.Getenv("F10_EXPECT_COMPOSE_INTEGRITY") != "1" {
		t.Skip("accepted risk until deploy/compose/docker-compose.yaml is covered by SHA256SUMS or a pinned digest; set F10_EXPECT_COMPOSE_INTEGRITY=1 to enforce")
	}
	src := readInstallSh(t)
	fetch := strings.Index(src, "deploy/compose/docker-compose.yaml")
	if fetch < 0 {
		t.Fatal("install.sh no longer fetches deploy/compose/docker-compose.yaml — update this guard")
	}
	after := src[fetch:]
	if !regexp.MustCompile(`docker-compose\.yaml[\s\S]{0,1200}(SHA256SUMS|sha256sum|shasum)[\s\S]{0,400}die "`).MatchString(after) {
		t.Errorf("install.sh fetches the compose file and never checks it against SHA256SUMS/a digest with a fail-closed die")
	}
}

// PINS the fall-through that turns an empty install-time token into a
// published credential: the compose file's `${WARDYN_ADMIN_TOKEN:-demo-admin-token}`
// (docker-compose.yaml:205) substitutes for BOTH unset and empty (Compose
// `:-` semantics), and resolveLocalMode boots on that token when the listen is
// the unspecified ":8080" the container uses (boot_flags.go:422-428;
// config_test.go:356 row). If either half changes, this test says so and the
// F10 trace doc needs a rewrite.
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
