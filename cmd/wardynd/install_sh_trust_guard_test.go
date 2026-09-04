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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func rawInstallSh(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	return string(b)
}

// installShCode is install.sh with its full-line comments blanked out. EVERY
// guard below asserts about CODE, and must therefore read this rather than the
// raw text.
//
// install.sh documents its own trust decisions at length, quoting the very
// commands it runs — "under `umask 077` both were dead code", "`shasum -a 256`
// on a mac-shaped PATH". A plain substring search was satisfied by that PROSE:
// deleting the executable `umask 077` and the executable `shasum -a 256`
// fallback each left its guard GREEN while scripts/test-install-sh-trust.sh
// went red, which is the whole failure mode these guards exist to prevent.
// Lines are blanked rather than dropped so offsets and (?m) anchors keep
// pointing at the same places.
func installShCode(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			b.WriteByte('\n')
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func readInstallSh(t *testing.T) string {
	t.Helper()
	return installShCode(rawInstallSh(t))
}

// adminTokenComplaints returns every way `code` fails to close H1/H2 — the two
// independent ways the admin-token mint used to yield a value that is not a
// secret. Pure so the counterfactual below can feed it a mutated script.
func adminTokenComplaints(code string) []string {
	var out []string
	tokLine := regexp.MustCompile(`(?m)^\s*TOKEN=\$\(.*$`).FindString(code)
	if tokLine == "" {
		return []string{"install.sh no longer derives TOKEN=$(...) — update this guard to the new derivation"}
	}
	if strings.Contains(tokLine, "sha256sum") && !strings.Contains(tokLine, "shasum") {
		out = append(out, "install.sh's admin-token line depends on `sha256sum` with no `shasum` fallback (sha256_hex, which install_cli's SHA256SUMS check uses, has one): on macOS TOKEN is empty and compose falls through to demo-admin-token\n  "+strings.TrimSpace(tokLine))
	}
	// H1: no hashing binary at all on a mac. sha256_hex's else-branch is the
	// ONLY line that runs there, so it is pinned as a COMMAND — until T2 of
	// scripts/test-install-sh-trust.sh was given a real mac-shaped PATH nothing
	// executed it, and the branch could have been deleted with every gate green.
	if !regexp.MustCompile(`(?m)^\s*shasum -a 256\b`).MatchString(code) {
		out = append(out, "install.sh has no `shasum -a 256` fallback anywhere: macOS ships no GNU coreutils, so every hashing site — the admin-token mint and install_cli's SHA256SUMS check — silently produces nothing there")
	}
	// H2: the MINT, not the hash. This is the assertion that closes it, and it
	// has to name the raw `docker run … -gen-age-key` output: TOKEN is a sha256
	// OUTPUT and is never empty (sha256("") is a 64-hex constant), so a
	// non-empty check on TOKEN cannot detect a failed mint — it was green with
	// mint_age_key's own guard deleted and sha256("")[0:48] installed as this
	// box's admin credential.
	if !regexp.MustCompile(`\[\s+-n\s+"\$\{?minted\}?"\s+\]\s*\|\|\s*die`).MatchString(code) {
		out = append(out, "install.sh's mint_age_key has no `[ -n \"$minted\" ] || die` guard on the raw `docker run … -gen-age-key` output: POSIX sh has no pipefail, so `set -e` cannot see the docker failure, and the empty stdin it leaves is hashed into sha256(\"\")[0:48] — a public constant — and installed as the admin token")
	}
	// The TOKEN non-empty guard is a SEPARATE, weaker check: it catches "no
	// hashing binary produced anything at all", not a failed mint.
	if !regexp.MustCompile(`\[\s+-n\s+"\$\{?TOKEN\}?"\s+\]\s*\|\|\s*die`).MatchString(code) {
		out = append(out, "install.sh has no `[ -n \"$TOKEN\" ] || die` guard: with no hashing binary on PATH the derivation yields an empty TOKEN and compose substitutes ${WARDYN_ADMIN_TOKEN:-demo-admin-token}")
	}
	return out
}

// envUnderRestrictiveUmask returns "" when `umask 077` runs, as a COMMAND,
// before the first `cat > .env`.
func envUnderRestrictiveUmask(code string) string {
	firstWrite := strings.Index(code, "cat > .env")
	if firstWrite < 0 {
		return "install.sh no longer writes .env via `cat > .env` — update this guard"
	}
	loc := regexp.MustCompile(`(?m)^\s*umask 077\s*$`).FindStringIndex(code)
	if loc == nil {
		return "install.sh writes .env with no `umask 077` COMMAND anywhere: the age key + admin token would be world-readable until a later chmod"
	}
	if loc[0] > firstWrite {
		return fmt.Sprintf("install.sh writes .env (offset %d) before its `umask 077` (offset %d): the age key + admin token would be world-readable until a later chmod", firstWrite, loc[0])
	}
	return ""
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
	for _, c := range adminTokenComplaints(readInstallSh(t)) {
		t.Error(c)
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
	if c := envUnderRestrictiveUmask(readInstallSh(t)); c != "" {
		t.Error(c)
	}
}

// TestInstallShGuards_AreNotSatisfiedByComments is the permanent counterfactual
// for the two guards above (F10 regression class REG1-001/REG1-002/TEST1-C1):
// each case DELETES the one executable line that closes a settled finding,
// leaves every comment that discusses it in place, and requires the guard to
// notice. Before the comment-stripping fix all three mutations left the Go
// guards green while scripts/test-install-sh-trust.sh went red.
func TestInstallShGuards_AreNotSatisfiedByComments(t *testing.T) {
	raw := rawInstallSh(t)
	for _, tc := range []struct {
		name  string
		cut   *regexp.Regexp
		fails func(code string) bool
	}{
		{
			name:  "umask 077 deleted, its comments kept",
			cut:   regexp.MustCompile(`(?m)^umask 077\n`),
			fails: func(code string) bool { return envUnderRestrictiveUmask(code) != "" },
		},
		{
			name:  "mint_age_key non-empty guard deleted",
			cut:   regexp.MustCompile(`(?m)^\s*\[ -n "\$minted" \] \|\| die .*\n`),
			fails: func(code string) bool { return len(adminTokenComplaints(code)) > 0 },
		},
		{
			name:  "shasum -a 256 fallback deleted",
			cut:   regexp.MustCompile(`(?m)^\s*shasum -a 256 .*\n`),
			fails: func(code string) bool { return len(adminTokenComplaints(code)) > 0 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Guard the guard: a mutation that matches nothing proves nothing.
			if !tc.cut.MatchString(raw) {
				t.Fatalf("the counterfactual's own pattern %s matches nothing in install.sh — the line it deletes moved or changed shape, and this case is now vacuous", tc.cut)
			}
			mutated := tc.cut.ReplaceAllString(raw, "")
			if !tc.fails(installShCode(mutated)) {
				t.Errorf("deleting the executable line left the guard GREEN — it is satisfied by install.sh's own COMMENTS, not by the command that closes the finding")
			}
			// Control: the real script must still pass, or the guard is simply broken.
			if tc.fails(installShCode(raw)) {
				t.Errorf("the guard fails on the UNMUTATED install.sh — the counterfactual proves nothing")
			}
		})
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
