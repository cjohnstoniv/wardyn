// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// tlsListenerViolations parses the given non-test sources (file name -> text)
// and returns every way they break the "one *tls.Config per listener" rule
// (#1312; the 0.7.9 shared-config regression). The rules:
//   - every http.Server literal that sets TLSConfig sets it from
//     tlsConfigForListener(...); the single exception is internal_tls.go's
//     hop.server, which is the internal CA's own config, not posture's;
//   - exactly 2 literals (serveAndShutdown, startUISandboxGateway) do so, so
//     a third listener added with a different shape is noticed here;
//   - nothing selects .tlsConfig outside tlsConfigForListener and
//     resolveTLSPosture.
func tlsListenerViolations(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	var out []string
	viaSeam, hopServer := 0, 0
	fset := token.NewFileSet()
	names := make([]string, 0, len(srcs))
	for n := range srcs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, srcs[name], 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Server" || identName(sel.X) != "http" {
						return true
					}
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok || identName(kv.Key) != "TLSConfig" {
							continue
						}
						pos := fset.Position(kv.Pos())
						switch v := kv.Value.(type) {
						case *ast.CallExpr:
							if identName(v.Fun) == "tlsConfigForListener" {
								viaSeam++
								continue
							}
						case *ast.SelectorExpr:
							if name == "internal_tls.go" && identName(v.X) == "hop" && v.Sel.Name == "server" {
								hopServer++
								continue
							}
						}
						out = append(out, fmt.Sprintf("%s: http.Server TLSConfig is not tlsConfigForListener(...)", pos))
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "tlsConfig" && (fn == nil || (fn.Name.Name != "tlsConfigForListener" && fn.Name.Name != "resolveTLSPosture")) {
						out = append(out, fmt.Sprintf("%s: .tlsConfig read outside tlsConfigForListener/resolveTLSPosture", fset.Position(x.Pos())))
					}
				}
				return true
			})
		}
	}
	if viaSeam != 2 {
		out = append(out, fmt.Sprintf("%d http.Server literals use tlsConfigForListener, want exactly 2", viaSeam))
	}
	if hopServer > 1 {
		out = append(out, fmt.Sprintf("%d http.Server literals use hop.server, want at most 1", hopServer))
	}
	return out
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// TestEveryTLSListenerGetsItsOwnConfig is the build-time half of
// TestListenersDoNotShareOneTLSConfig: that test proves the seam clones; this
// one proves every listener goes through the seam, in every build flavor's
// non-test source, so a new http.Server cannot be handed posture.tlsConfig.
func TestEveryTLSListenerGetsItsOwnConfig(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		srcs[f] = string(b)
	}
	if len(srcs) == 0 {
		t.Fatal("no non-test sources found; the guard would pass vacuously")
	}
	if v := tlsListenerViolations(t, srcs); len(v) != 0 {
		t.Fatalf("TLS listener config violations:\n%s", strings.Join(v, "\n"))
	}

	t.Run("sharing posture.tlsConfig is caught", func(t *testing.T) {
		const bad = `package main

import "net/http"

func tlsConfigForListener(p tlsPosture) *tlsConfigT { return p.tlsConfig.Clone() }

func a(posture tlsPosture) { _ = &http.Server{TLSConfig: tlsConfigForListener(posture)} }
func b(posture tlsPosture) { _ = &http.Server{TLSConfig: tlsConfigForListener(posture)} }
func c(posture tlsPosture) { _ = &http.Server{TLSConfig: posture.tlsConfig} }
`
		v := tlsListenerViolations(t, map[string]string{"bad.go": bad})
		if len(v) < 2 {
			t.Fatalf("guard accepted TLSConfig: posture.tlsConfig; violations = %v", v)
		}
		if !strings.Contains(strings.Join(v, "\n"), "is not tlsConfigForListener") {
			t.Errorf("violations do not name the literal: %v", v)
		}
	})
}
