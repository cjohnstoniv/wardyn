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

// localCoveredWrites lists every place f assigns Server.localCovered: a selector assignment, a
// composite-literal key, or an address taken (&x.localCovered). The declaration in server.go and the
// read in promotionCovered are neither, so they pass.
func localCoveredWrites(fset *token.FileSet, f *ast.File) []string {
	var out []string
	report := func(p token.Pos, how string) {
		out = append(out, fset.Position(p).String()+": "+how+" localCovered")
	}
	isField := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "localCovered"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				if isField(l) {
					report(l.Pos(), "assigns")
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && id.Name == "localCovered" {
				report(x.Pos(), "sets, as a literal key,")
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND && isField(x.X) {
				report(x.Pos(), "takes the address of")
			}
		}
		return true
	})
	return out
}

// TestLocalCoveredIsNeverAssignedInProduction: Server.localCovered replaces the mask-coverage proof
// (maskCovered) for a local promotion, so any non-test code that sets it would let a run whose corpus
// is not provably whole be promoted to writer. It exists for tests only (they set it on a Server they
// built); the declaration in server.go and the read in promotionCovered are the only production uses.
func TestLocalCoveredIsNeverAssignedInProduction(t *testing.T) {
	fset := token.NewFileSet()
	scanned := 0
	for _, dir := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			for _, v := range localCoveredWrites(fset, f) {
				t.Errorf("%s\nServer.localCovered overrides the mask-coverage proof and must stay test-only: production code must never set it", v)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files")
	}
}

// TestLocalCoveredGuardFiresOnAPlantedWrite shows the guard is not satisfied by being silent.
func TestLocalCoveredGuardFiresOnAPlantedWrite(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{"a selector assignment", `package api
func f(s *Server) { s.localCovered = func(uuid.UUID) bool { return true } }`, 1},
		{"a composite-literal key", `package api
func f() *Server { return &Server{localCovered: nil} }`, 1},
		{"an address taken for writing", `package api
func f(s *Server) { p := &s.localCovered; *p = nil }`, 1},
		{"the declaration passes", `package api
type Server struct{ localCovered func(runID uuid.UUID) bool }`, 0},
		{"the read passes", `package api
func (s *Server) f(id uuid.UUID) bool { if s.localCovered != nil { return s.localCovered(id) }; return false }`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "planted.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(localCoveredWrites(fset, f)); got != tc.want {
				t.Errorf("%d writes reported, want %d", got, tc.want)
			}
		})
	}
}
