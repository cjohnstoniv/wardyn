// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envDocRoots are the non-test Go trees whose WARDYN_* reads must stay in sync
// with docs/ENV.md. Every WARDYN_* literal read here has to be documented, and
// every documented WARDYN_* row has to have a reader here (or be allowlisted as
// test-only). Together these two directions cover the full var surface.
var envDocRoots = []string{"cmd", "internal"}

// envDocAllow lists test-scaffolding / harness / negative-control vars: not
// operator config, so intentionally not in the registry tables. They are read
// only from _test.go (or documented purely as test-only), so they are excluded
// from BOTH parity directions. Keep in sync with ENV.md's "Test / internal-only"
// section.
var envDocAllow = map[string]bool{
	"WARDYN_TEST_BOOL": true, "WARDYN_TEST_DUR": true, "WARDYN_TEST_STR": true,
	"WARDYN_TEST_PG": true, "WARDYN_TEST_DOCKER": true, "WARDYN_TEST_CACHE_REPO": true,
	"WARDYN_TEST_TOOLS_DIR": true, "WARDYN_ENVBUILD_TEST_FLOAT": true,
	"WARDYN_ENVBUILD_TEST_INT": true, "WARDYN_FAKE_MARKER": true, "WARDYN_NEGCTL": true,
	"WARDYN_E2E_BASE_URL": true, "WARDYN_E2E_CLAUDE_CREDS": true,
	"WARDYN_E2E_REAL_MODEL": true, "WARDYN_E2E_TASKS_DIR": true,
	"WARDYN_E2E_WORK_ROOT": true, "WARDYN_E2E_EXPECT_INJECT": true,
	// k8s conformance suite gating (test/conformance/conformance_k8s_test.go):
	// outside envDocRoots (test/, not cmd/ or internal/) so the forward ratchet
	// never sees these anyway, but listed here too (and in ENV.md's Test/
	// internal-only section) so the allowlist itself stays the honest, complete
	// registry of every test-only WARDYN_* var, not just the ones the mechanical
	// scan happens to reach.
	"WARDYN_TEST_K8S": true, "WARDYN_TEST_K8S_AGENT_IMAGE": true,
	// The Playwright e2e backend's two listen addresses (scripts/e2e-backend.sh):
	// the console's and the UI-sandbox gateway's, which must differ. Shell-only
	// like the block below, but test scaffolding rather than operator config, so
	// they belong here.
	"WARDYN_E2E_ADDR": true, "WARDYN_E2E_UI_ADDR": true,
}

// envDocShellOnly lists vars read ONLY by deploy/compose/docker-compose.yaml and
// the operator scripts, never by Go. They are real operator configuration (so
// they belong in ENV.md's "Compose / scripts" section), but the reverse ratchet
// below would flag them as stale rows since no Go file reads them. Keep in sync
// with that section.
var envDocShellOnly = map[string]bool{
	"WARDYN_NS": true, "WARDYN_UP_PORT": true, "WARDYN_PG_PORT": true, "WARDYN_DEX_PORT": true,
	"WARDYN_CI_PROJECT": true, "WARDYN_REGISTRY_PORT": true, "WARDYN_SSH_PORT": true,
	// The compose host-port mapping for the UI-sandbox gateway — WARDYN_SSH_PORT's
	// sibling, and a mapping only: what enables the gateway is
	// WARDYN_UI_SANDBOX_LISTEN, which Go does read.
	"WARDYN_UI_SANDBOX_PORT": true,
	// UI build stage + its cross-compile targets: read by scripts/up.sh and
	// interpolated by docker-compose.yaml into build args, never by Go.
	"WARDYN_UI_STAGE": true, "WARDYN_HOST_GOOS": true, "WARDYN_HOST_GOARCH": true,
	// Forces scripts/up.sh to build every image from the working tree instead of
	// pulling the published ones. An installer-time decision, so no Go reads it.
	"WARDYN_BUILD_LOCAL": true,
	// The compose wardynd service's own image tag, so a job on a shared daemon
	// can build its own instead of racing another job's write to the mutable
	// :local tag (scripts/run-e2e-ssh.sh, run-e2e-ui-sandbox.sh). Its sibling
	// WARDYN_PROXY_IMAGE is NOT here: Go reads that one (-proxy-image).
	"WARDYN_WARDYND_IMAGE": true,
	// Compose/runner plumbing and the `make setup` installer: read by
	// docker-compose.yaml, scripts/setup.sh, scripts/up.sh and scripts/ci-run.sh,
	// never by Go. Documented in ENV.md's "Compose / scripts" + "Setup / operator
	// scripts" sections.
	"WARDYN_CI_TOOLS_DIR": true, "WARDYN_DOCKER_SOCK": true, "WARDYN_WORKSPACES_ROOT": true,
	// The compose-side, SINGULAR sibling of WARDYN_USER_DRIVE_HOST_ROOTS (which
	// Go does read): docker-compose.yaml binds this one host tree read-only at
	// the same path inside the wardynd container so the drive ceiling's
	// EvalSymlinks can run there. A mount mapping, never a ceiling — nothing in
	// Go reads it, by design.
	"WARDYN_USER_DRIVE_HOST_ROOT": true,
	// WARDYN_STAGE_CLAUDE is deliberately NOT here any more: it had an ENV.md
	// row, an exemption on this list, and no reader ANYWHERE — not in Go, not in
	// a script — so the exemption was the only thing keeping a dead row green,
	// while this list's own stated predicate ("read by compose/the operator
	// scripts") was false for it. The row is gone with it. If the knob is ever
	// implemented in scripts/setup.sh's staging branch (the sibling of the
	// WARDYN_IMPORT_AWS / WARDYN_IMPORT_SCM gates), document it and re-add it.
	"WARDYN_SETUP_MODE": true, "WARDYN_SUBSCRIPTION_TOKEN": true,
	"WARDYN_IMPORT_AWS": true, "WARDYN_IMPORT_SCM": true, "WARDYN_FORCE_RESET": true,
	"WARDYN_DEFAULT_POLICY_AUTO": true,
	// The rest of the operator knobs install.sh + scripts/*.sh actually read.
	// Real configuration, documented in ENV.md's "Setup / operator scripts
	// (shell-only)" table, with no Go reader by design.
	"WARDYN_FORCE_STOP_HOST": true, "WARDYN_UP_NO_BROWSER": true,
	"WARDYN_UP_SKIP_RUN_IMAGES": true, "WARDYN_SCM_SSH_HOSTS": true,
	"WARDYN_GEN_DEPLOY_KEY": true, "WARDYN_DEPLOY_KEY_HOST": true,
	// install.sh's own three: the one-line installer's version pin, its target
	// directory, and the loopback port it publishes.
	"WARDYN_VERSION": true, "WARDYN_HOME": true, "WARDYN_PORT": true,
	// The desktop tier's MDM-managed directory: docker-compose.yaml bind-mounts
	// it read-only at /etc/wardyn and wardyn-desktop.sh exports it. Go never
	// reads the var — it reads the POLICY FILE at the path inside that mount
	// (WARDYN_DEFAULT_POLICY), which is a separate, Go-read var.
	"WARDYN_MANAGED_DIR": true,
	// The desktop-tier installer's own image override — read only by
	// deploy/desktop/install.sh (`-gen-age-key`), never by Go. Documented in
	// ENV.md's "Setup / operator scripts" section.
	"WARDYN_INSTALL_IMAGE": true,
}

var wardynVarLit = regexp.MustCompile(`WARDYN_[A-Z0-9_]+`)

// readVars returns every WARDYN_* string literal read in non-test .go under the
// envDocRoots — the full read surface the docs must cover.
func readVars(t *testing.T, root string) map[string]bool {
	t.Helper()
	quoted := regexp.MustCompile(`"WARDYN_[A-Z0-9_]+"`)
	seen := map[string]bool{}
	for _, sub := range envDocRoots {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range quoted.FindAllString(string(b), -1) {
				seen[strings.Trim(m, `"`)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	return seen
}

// documentedVars tokenizes docs/ENV.md into the exact WARDYN_* names it names.
// The regex is greedy, so WARDYN_GROUNDTRUTH_TOKEN_FILE yields exactly that one
// token and NOT its prefix WARDYN_GROUNDTRUTH_TOKEN — which is the whole point:
// both ratchets must compare whole names, never substrings.
func documentedVars(docText string) map[string]bool {
	documented := map[string]bool{}
	for _, m := range wardynVarLit.FindAllString(docText, -1) {
		documented[m] = true
	}
	return documented
}

// envDocForwardMissing is the forward ratchet's decision, split out from the
// test so a fixture doc can exercise it. Returns the Go-read vars docs/ENV.md
// does not name, sorted.
//
// R5 F211: this used to be strings.Contains over the whole document, which any
// var that is a strict PREFIX of another documented var passed with its own row
// deleted — ten of them, including the secret WARDYN_GROUNDTRUTH_TOKEN
// (absorbed by WARDYN_GROUNDTRUTH_TOKEN_FILE). Compare against the same
// tokenized name set the reverse ratchet already builds.
func envDocForwardMissing(read map[string]bool, docText string) []string {
	documented := documentedVars(docText)
	var missing []string
	for v := range read {
		if envDocAllow[v] {
			continue
		}
		if !documented[v] {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	return missing
}

// TestEnvDoc_ForwardEveryReadIsDocumented ratchets one direction: every WARDYN_*
// literal read in non-test Go under cmd/ + internal/ must appear in docs/ENV.md
// (or the test-only allowlist). Adds a new env var without documenting it → fail.
func TestEnvDoc_ForwardEveryReadIsDocumented(t *testing.T) {
	root := repoRoot(t)
	for _, v := range envDocForwardMissing(readVars(t, root), readEnvDoc(t, root)) {
		t.Errorf("%s is read in non-test Go but undocumented in docs/ENV.md (add it there, or to envDocAllow if it is test-only)", v)
	}
}

// TestEnvDoc_ForwardRejectsPrefixAbsorbedRow is the counterfactual the forward
// ratchet could not make: delete the ONE docs/ENV.md row for a Go-read var that
// is a strict prefix of another documented var, and the ratchet must notice.
// WARDYN_GROUNDTRUTH_TOKEN (read by cmd/wardyn-tetragon-ingest) is the live
// example — it is a host-sensor bearer token, so an undocumented one is exactly
// the row an operator must not lose silently.
func TestEnvDoc_ForwardRejectsPrefixAbsorbedRow(t *testing.T) {
	const absorbed = "WARDYN_GROUNDTRUTH_TOKEN"
	root := repoRoot(t)
	read := readVars(t, root)
	if !read[absorbed] {
		t.Fatalf("%s is no longer read in non-test Go — repoint this counterfactual at another prefix-absorbed var", absorbed)
	}

	// Drop only that var's own row(s); the WARDYN_GROUNDTRUTH_TOKEN_FILE row
	// that used to absorb it stays, which is what makes this a counterfactual.
	var kept []string
	dropped := 0
	for _, line := range strings.Split(readEnvDoc(t, root), "\n") {
		if strings.HasPrefix(line, "| `"+absorbed+"`") {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped == 0 {
		t.Fatalf("no docs/ENV.md row starts with | `%s` — the fixture no longer removes anything", absorbed)
	}
	fixture := strings.Join(kept, "\n")
	if !strings.Contains(fixture, absorbed) {
		t.Fatalf("fixture no longer contains %s as a substring, so it cannot prove the substring check was the bug", absorbed)
	}

	missing := envDocForwardMissing(read, fixture)
	found := false
	for _, v := range missing {
		if v == absorbed {
			found = true
		}
	}
	if !found {
		t.Errorf("forward ratchet passed docs/ENV.md with %s's row deleted (missing=%v) — it is matching substrings, not documented names: %s is still present inside %s_FILE", absorbed, missing, absorbed, absorbed)
	}
}

// TestEnvDoc_ReverseEveryRowHasReader ratchets the other direction: every
// WARDYN_* row in docs/ENV.md must still have a live reader in the tree.
// Deleting the last reader of a var but leaving its row → fail. Prevents doc rot
// (stale rows that outlive the code that read them).
func TestEnvDoc_ReverseEveryRowHasReader(t *testing.T) {
	root := repoRoot(t)
	docText := readEnvDoc(t, root)
	seen := readVars(t, root)

	documented := documentedVars(docText)
	for v := range documented {
		if envDocAllow[v] {
			continue // documented purely as test-only; read only from _test.go
		}
		if envDocShellOnly[v] {
			continue // compose/scripts config; no Go reader by design
		}
		if !seen[v] {
			t.Errorf("%s has a docs/ENV.md row but no reader in non-test Go under %v — delete the stale row (or add it to envDocAllow if it is test-only, or envDocShellOnly if compose/scripts read it)", v, envDocRoots)
		}
	}
}

func readEnvDoc(t *testing.T, root string) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join(root, "docs", "ENV.md"))
	if err != nil {
		t.Fatalf("read docs/ENV.md: %v", err)
	}
	return string(doc)
}

// repoRoot walks up from the test's working directory (the package dir) to the
// dir holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}
