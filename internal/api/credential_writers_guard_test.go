// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// operatorNamespaceWriters are the only functions that call a secret store's Put outside
// putOwned: the daemon's own boot keys, written into the operator namespace, which no erase
// reaches. A person's credential is never written from here. Keys are "file:function".
var operatorNamespaceWriters = map[string]bool{
	"cmd/wardynd/boot_keys.go:loadOrCreateSecret": true,
	"cmd/wardynd/boot_hybrid.go:bootHybrid":       true,
}

// TestCredentialWritersRouteThroughPutOwned pins the one-path rule of #1511:
// every write into a person's secret namespace is made inside putOwned's
// callback, which hands it a context fenced to the owner's erase generation. A
// writer that called Put directly would not be refused after an erase.
func TestCredentialWritersRouteThroughPutOwned(t *testing.T) {
	root := filepath.Join("..", "..")
	var direct, unfenced []string
	routed := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() && (strings.HasPrefix(rel, "internal/secretstore") || strings.Contains(rel, "testdata")) {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return perr
			}
			d2, u2, r2 := credentialWriterFindings(f, rel)
			direct, unfenced, routed = append(direct, d2...), append(unfenced, u2...), routed+r2
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range direct {
		t.Errorf("%s writes a secret with Put outside putOwned: route it through putOwned so an erase of its owner refuses it (secretstore.BeginWrite)", at)
	}
	for _, at := range unfenced {
		t.Errorf("%s: a Put inside putOwned must take the callback's own context, the one fenced to the erase generation", at)
	}
	if routed == 0 {
		t.Fatal("found no Put inside putOwned: the guard is not looking at the writers")
	}
}

// The guard sees every shape of a secret-store write: a Put of any arity, a Put taken as a method
// value, one off the callback's context, and a Put in a function the allowlist does not name.
func TestCredentialWriterGuardCatchesEveryShape(t *testing.T) {
	for name, tc := range map[string]struct {
		src          string
		direct       []string
		unfenced     []string
		routed       int
		allowedFuncs []string
	}{
		"fenced Put":        {src: "func f() { s.putOwned(func(c C) { s.Put(c, n, v) }) }", routed: 1},
		"direct Put":        {src: "func f() { s.Put(ctx, n, v) }", direct: []string{"x.go:f"}},
		"four argument Put": {src: "func f() { s.Put(ctx, n, v, opt) }", direct: []string{"x.go:f"}},
		"method value":      {src: "func f() { w := s.Put; w(ctx, n, v) }", direct: []string{"x.go:f"}},
		"router Put":        {src: `func f() { r.Put("/x", h) }`},
		"other context":     {src: "func f() { s.putOwned(func(c C) { s.Put(ctx, n, v) }) }", unfenced: []string{"x.go"}, routed: 1},
		"method value in a callback": {
			src: "func f() { s.putOwned(func(c C) { w := s.Put; w(c, n, v) }) }", unfenced: []string{"x.go"}, routed: 1},
		"four argument Put in a callback": {
			src: "func f() { s.putOwned(func(c C) { s.Put(c, n, v, opt) }) }", routed: 1},
		"allowed function":      {src: "func f() { s.Put(ctx, n, v) }", allowedFuncs: []string{"x.go:f"}},
		"another function":      {src: "func f() { s.Put(ctx, n, v) }\nfunc g() { s.Put(ctx, n, v) }", direct: []string{"x.go:g"}, allowedFuncs: []string{"x.go:f"}},
		"allowlist is per func": {src: "func f() {}\nfunc g() { s.Put(ctx, n, v) }", direct: []string{"x.go:g"}, allowedFuncs: []string{"x.go:f"}},
		"package variable":      {src: "var w = s.Put", direct: []string{"x.go:"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			saved := operatorNamespaceWriters
			operatorNamespaceWriters = map[string]bool{}
			for _, k := range tc.allowedFuncs {
				operatorNamespaceWriters[k] = true
			}
			defer func() { operatorNamespaceWriters = saved }()
			direct, unfenced, routed := credentialWriterFindings(f, "x.go")
			if !slices.Equal(direct, tc.direct) || !slices.Equal(unfenced, tc.unfenced) || routed != tc.routed {
				t.Errorf("direct %v unfenced %v routed %d; want %v %v %d", direct, unfenced, routed, tc.direct, tc.unfenced, tc.routed)
			}
		})
	}
}

// credentialWriterFindings reads one file: the "file:function" of each declaration with a Put outside
// putOwned that operatorNamespaceWriters does not name, each Put inside a putOwned callback that does
// not take the callback's own context, and how many Puts it found inside putOwned.
func credentialWriterFindings(f *ast.File, rel string) (direct, unfenced []string, routed int) {
	for _, decl := range f.Decls {
		fn := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		inside := 0
		ast.Inspect(decl, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isPutOwnedCall(call) {
				for _, a := range call.Args {
					if lit, ok := a.(*ast.FuncLit); ok {
						inside += countStorePuts(lit)
						unfenced = append(unfenced, putsOffCallbackContext(lit, rel)...)
					}
				}
			}
			return true
		})
		routed += inside
		if countStorePuts(decl) > inside && !operatorNamespaceWriters[rel+":"+fn] {
			direct = append(direct, rel+":"+fn)
		}
	}
	return direct, unfenced, routed
}

func isPutOwnedCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "putOwned"
}

// storePuts finds n's secret-store writes: each .Put call whose first argument is not a string (a
// router's Put(path, handler) names a path), whatever its arity, and each .Put taken without a call, a
// method value that could be called later with any context.
func storePuts(n ast.Node) (calls []*ast.CallExpr, values []*ast.SelectorExpr) {
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Put" {
			called[sel] = true
			if lit, ok := firstArg(call).(*ast.BasicLit); !ok || lit.Kind != token.STRING {
				calls = append(calls, call)
			}
		}
		return true
	})
	ast.Inspect(n, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Put" && !called[sel] {
			values = append(values, sel)
		}
		return true
	})
	return calls, values
}

func firstArg(call *ast.CallExpr) ast.Expr {
	if len(call.Args) == 0 {
		return nil
	}
	return call.Args[0]
}

// countStorePuts counts n's secret-store writes, calls and method values.
func countStorePuts(n ast.Node) int {
	calls, values := storePuts(n)
	return len(calls) + len(values)
}

// putsOffCallbackContext names each Put in a putOwned callback whose first argument is not the callback's
// own context parameter, and each method value of Put: any other context carries no fence.
func putsOffCallbackContext(lit *ast.FuncLit, rel string) []string {
	if len(lit.Type.Params.List) == 0 || len(lit.Type.Params.List[0].Names) == 0 {
		return []string{rel + " (a callback with no context parameter)"}
	}
	ctxName := lit.Type.Params.List[0].Names[0].Name
	var off []string
	calls, values := storePuts(lit)
	for _, call := range calls {
		if id, ok := firstArg(call).(*ast.Ident); !ok || id.Name != ctxName {
			off = append(off, rel)
		}
	}
	for range values {
		off = append(off, rel)
	}
	return off
}
