// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentUIDIsTheOnlyUIDLiteral: the agent's uid is written once, as
// AgentUID, and every substrate reads it from there. A literal 1000, "1000" or
// "1000:1000" anywhere else in this package's or a substrate's production
// files is a second copy that could drift from it. A 1000 multiplied or divided
// by is a unit conversion, not a uid.
func TestAgentUIDIsTheOnlyUIDLiteral(t *testing.T) {
	var files []string
	for _, dir := range []string{".", "docker", "k8s"} {
		m, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	isUID := func(lit *ast.BasicLit) bool {
		return lit.Value == "1000" || lit.Value == `"1000"` || lit.Value == `"1000:1000"`
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		allowed := map[ast.Expr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for i, v := range n.Values {
					if i < len(n.Names) && n.Names[i].Name == "AgentUID" && filepath.Dir(name) == "." {
						allowed[v] = true
					}
				}
			case *ast.BinaryExpr:
				if n.Op == token.MUL || n.Op == token.QUO {
					allowed[n.X], allowed[n.Y] = true, true
				}
			case *ast.BasicLit:
				if isUID(n) && !allowed[n] {
					t.Errorf("%s: a uid literal; use AgentUID", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	if len(files) == 0 {
		t.Fatal("no files scanned")
	}
}
