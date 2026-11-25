// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutageWindowWritersAreTheCPsOwn pins who may open, close, heal or prune an outage window (#4082): only the
// control plane's own observations, in cp_heartbeat.go. Each of its helpers has its one call site: the heartbeat
// loop in ReconcileOnBoot, the two runner-link hooks in the runner hub, and the server window in the lapse sweep.
// Readers (OutageWindowCovers) are not restricted.
func TestOutageWindowWritersAreTheCPsOwn(t *testing.T) {
	writers := map[string]bool{"BeatControlPlane": true, "OpenRunnerLinkWindow": true, "CloseRunnerLinkWindow": true,
		"HealRunnerLinkWindows": true, "OpenServerOutageWindow": true, "CloseServerOutageWindow": true, "PruneOutageWindows": true}
	helpers := map[string]string{
		"runCPHeartbeat":           "internal/api/reconcile.go",
		"openRunnerLinkWindow":     "internal/api/runner_stream.go",
		"closeRunnerLinkWindow":    "internal/api/runner_stream.go",
		"recordServerOutageWindow": "internal/api/run_lost.go",
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	helperSites := map[string][]string{}
	writerSites := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if writers[name] {
					writerSites++
					if rel != "internal/api/cp_heartbeat.go" {
						t.Errorf("%s: %s writes an outage window outside cp_heartbeat.go", fset.Position(sel.Pos()), name)
					}
				}
				if _, ok := helpers[name]; ok {
					helperSites[name] = append(helperSites[name], rel)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if writerSites == 0 {
		t.Fatal("no outage-window writer call found: the walk read nothing")
	}
	for name, want := range helpers {
		if got := helperSites[name]; len(got) != 1 || got[0] != want {
			t.Errorf("%s is referenced from %v; want once, from %s", name, got, want)
		}
	}
}
