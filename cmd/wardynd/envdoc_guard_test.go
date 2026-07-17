// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
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

// TestEnvDoc_ForwardEveryReadIsDocumented ratchets one direction: every WARDYN_*
// literal read in non-test Go under cmd/ + internal/ must appear in docs/ENV.md
// (or the test-only allowlist). Adds a new env var without documenting it → fail.
func TestEnvDoc_ForwardEveryReadIsDocumented(t *testing.T) {
	root := repoRoot(t)
	docText := readEnvDoc(t, root)

	for v := range readVars(t, root) {
		if envDocAllow[v] {
			continue
		}
		if !strings.Contains(docText, v) {
			t.Errorf("%s is read in non-test Go but undocumented in docs/ENV.md (add it there, or to envDocAllow if it is test-only)", v)
		}
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

	documented := map[string]bool{}
	for _, m := range wardynVarLit.FindAllString(docText, -1) {
		documented[m] = true
	}
	for v := range documented {
		if envDocAllow[v] {
			continue // documented purely as test-only; read only from _test.go
		}
		if !seen[v] {
			t.Errorf("%s has a docs/ENV.md row but no reader in non-test Go under %v — delete the stale row (or add it to envDocAllow if it is test-only)", v, envDocRoots)
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
