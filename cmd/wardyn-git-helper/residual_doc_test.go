// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResidualDoc_MintRouteHonesty guards against W19-W19a-5 regressing: the
// package doc's "Caller authentication" RESIDUAL paragraph, and
// THREAT-MODEL.md's git_pat row, must both say plainly that the per-run
// caller-auth secret only binds a caller going through THIS BINARY, and that
// the proxy's local mint route (POST /wardyn/v1/credentials/mint) is itself
// unauthenticated — so a caller that reads the grant id straight out of the
// container-wide sandbox env and POSTs the route directly is not bound at
// all. Losing either mention re-opens the "claims a bar it doesn't raise"
// gap this test exists to catch.
func TestResidualDoc_MintRouteHonesty(t *testing.T) {
	root := repoRootForTest(t)

	src, err := os.ReadFile(filepath.Join(root, "cmd", "wardyn-git-helper", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	mustContain(t, "cmd/wardyn-git-helper/main.go", string(src),
		"unauthenticated",
		"/wardyn/v1/credentials/mint",
		"not bound",
	)

	tm, err := os.ReadFile(filepath.Join(root, "threatmodel", "THREAT-MODEL.md"))
	if err != nil {
		t.Fatalf("read THREAT-MODEL.md: %v", err)
	}
	gitPATRow := gitPATRowFrom(t, string(tm))
	mustContain(t, "threatmodel/THREAT-MODEL.md git_pat row", gitPATRow,
		"unauthenticated",
		"/wardyn/v1/credentials/mint",
		"not bound",
	)
}

func mustContain(t *testing.T, label, text string, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		if !strings.Contains(text, s) {
			t.Errorf("%s: expected honest-claims text to contain %q, it does not", label, s)
		}
	}
}

// gitPATRowFrom extracts the `git_pat` grant table row from the
// THREAT-MODEL.md §5.1a exceptions table.
func gitPATRowFrom(t *testing.T, doc string) string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| `git_pat` grant") {
			return line
		}
	}
	t.Fatal("THREAT-MODEL.md: no `git_pat` grant row found in §5.1a table")
	return ""
}

// repoRootForTest walks up from the package dir to the dir holding go.mod.
func repoRootForTest(t *testing.T) string {
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
