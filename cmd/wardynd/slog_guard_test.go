// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// slogOnlyPackages are the daemon/sidecar packages whose PRIMARY operator-facing
// logging surface has been migrated off the unstructured `log` package onto
// log/slog with typed attrs (the pattern in internal/audit/sink.go). They must
// stay migrated: `log.Printf` emits an opaque line an operator cannot filter or
// ship to a structured sink, so a single reintroduced call quietly re-opens the
// hole this guard exists to keep shut.
//
// Checking the IMPORT rather than grepping for "log.Printf" is what makes this
// airtight: an import alias (`import stdlog "log"`) or a fmt.Fprintf to
// os.Stderr-style workaround would slip past a textual scan, but no file can use
// the stdlib log package without importing it. `log/slog` is a distinct path and
// is unaffected.
//
// ponytail: this repo has no golangci-lint/forbidigo (make lint == go vet), so a
// small test is the only ratchet available. Add a linter and delete this.
var slogOnlyPackages = []string{
	"cmd/wardynd",
	"cmd/wardyn-tetragon-ingest",
	"cmd/wardyn-proxy",
	"internal/approval",
	"internal/runner/docker",
	"internal/api",
	"internal/egress/proxy",
}

func TestSlogOnlyPackagesDoNotImportStdlibLog(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	// Guard the guard: a wrong root would silently scan nothing and pass.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}

	scanned := 0
	for _, pkg := range slogOnlyPackages {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			// ParseFile with ImportsOnly still honors build tags in the header
			// comment only via the go/build package, so parse unconditionally:
			// -tags docker files (runner_docker.go, hardening.go) are part of the
			// migrated surface and must be checked too.
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			scanned++
			for _, imp := range f.Imports {
				if imp.Path.Value == `"log"` {
					t.Errorf("%s/%s imports the stdlib %q package; use log/slog with typed attrs "+
						"(slog.String/slog.Int/slog.Duration...) — see internal/audit/sink.go", pkg, name, "log")
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned 0 files — the package list or module root is wrong")
	}
	t.Logf("scanned %d non-test files across %d packages", scanned, len(slogOnlyPackages))
}
