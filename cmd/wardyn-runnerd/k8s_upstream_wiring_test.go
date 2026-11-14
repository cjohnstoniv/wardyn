// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestK8sRunnerHandsItsUpstreamProxyToTheDriver pins the k8s runner's --upstream-proxy wiring: the
// driver config k8sRunnerHooks builds carries it, as production_docker.go's does, so every run's proxy
// chains through it rather than going out direct. The k8s build is hosted-only, so this reads the source.
func TestK8sRunnerHandsItsUpstreamProxyToTheDriver(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "production_k8s.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "k8sRunnerHooks" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 && isSelectorOf(as.Lhs[0], "c", "UpstreamProxyURL") && isSelectorOf(as.Rhs[0], "opts", "upstreamProxy") {
				wired++
			}
			return true
		})
	}
	if wired != 1 {
		t.Fatalf("k8sRunnerHooks sets c.UpstreamProxyURL = opts.upstreamProxy %d times, want exactly 1", wired)
	}
}
