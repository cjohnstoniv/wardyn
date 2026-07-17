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

// TestEnvDoc_EveryVarDocumented is a ratchet: every WARDYN_* string literal read
// in non-test Go must appear in docs/ENV.md (or the test/internal allowlist).
// Adds a new env var without documenting it → this fails. Keeps ENV.md honest.
func TestEnvDoc_EveryVarDocumented(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "ENV.md"))
	if err != nil {
		t.Fatalf("read docs/ENV.md: %v", err)
	}
	docText := string(doc)

	// Test-scaffolding / harness / negative-control vars: not operator config, so
	// intentionally not in the registry table. Keep in sync with ENV.md's
	// "Test / internal-only" section.
	allow := map[string]bool{
		"WARDYN_TEST_BOOL": true, "WARDYN_TEST_DUR": true, "WARDYN_TEST_STR": true,
		"WARDYN_TEST_PG": true, "WARDYN_TEST_DOCKER": true, "WARDYN_TEST_CACHE_REPO": true,
		"WARDYN_TEST_TOOLS_DIR": true, "WARDYN_ENVBUILD_TEST_FLOAT": true,
		"WARDYN_ENVBUILD_TEST_INT": true, "WARDYN_FAKE_MARKER": true, "WARDYN_NEGCTL": true,
		"WARDYN_E2E_BASE_URL": true, "WARDYN_E2E_CLAUDE_CREDS": true,
		"WARDYN_E2E_REAL_MODEL": true, "WARDYN_E2E_TASKS_DIR": true,
		"WARDYN_E2E_WORK_ROOT": true, "WARDYN_E2E_EXPECT_INJECT": true,
	}

	lit := regexp.MustCompile(`"WARDYN_[A-Z0-9_]+"`)
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
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
		for _, m := range lit.FindAllString(string(b), -1) {
			seen[strings.Trim(m, `"`)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	for v := range seen {
		if allow[v] {
			continue
		}
		if !strings.Contains(docText, v) {
			t.Errorf("%s is read in non-test Go but undocumented in docs/ENV.md (add it there, or to the allowlist if it is test-only)", v)
		}
	}
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
