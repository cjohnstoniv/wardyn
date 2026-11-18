// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// relayKeyGuardDirs are the server-side packages the source guard reads, relative to this package.
var relayKeyGuardDirs = []string{"../api", "../store", "../types", "../../cmd/wardynd"}

// TestRelayKeyNeverSetServerSide holds the premise behind the placement pin of
// proxy.Config.RelayKey: no server-side code sets it, so a stored config never
// carries one. If it did, the three store writes (run_proxy_config, run_egress_edit,
// run_policy_generation) and refreshLocalDeploymentConfig would re-encode it
// unexamined. The guard fails on any mention of the field, its JSON name, or a
// literal containing relay_key in those packages. A new legitimate use needs a
// placement row (a refusal or a strip), not an exemption here.
func TestRelayKeyNeverSetServerSide(t *testing.T) {
	for _, dir := range relayKeyGuardDirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		parsed := 0
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", f, err)
			}
			parsed++
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == "RelayKey" {
						t.Errorf("%s: selector RelayKey", fset.Position(x.Sel.Pos()))
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "RelayKey" {
						t.Errorf("%s: composite key RelayKey", fset.Position(id.Pos()))
					}
				case *ast.BasicLit:
					if x.Kind != token.STRING {
						break
					}
					if s, err := strconv.Unquote(x.Value); err == nil && strings.Contains(strings.ToLower(s), "relay_key") {
						t.Errorf("%s: string literal mentions relay_key", fset.Position(x.Pos()))
					}
				}
				return true
			})
		}
		if parsed == 0 {
			t.Errorf("%s: read 0 non-test .go files, the guard would be vacuous", dir)
		}
	}
}

// TestRelayKeyNeverSetServerSide's second half: the dispatched runner.ProxyConfig has no relay key field.
func TestRelayKeyNeverSetServerSide_ProxyConfigShape(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			p := path + "." + f.Name
			if f.Name == "RelayKey" {
				t.Errorf("%s: field named RelayKey", p)
			}
			if strings.HasPrefix(strings.Split(f.Tag.Get("json"), ",")[0], "relay_key") {
				t.Errorf("%s: json tag %q", p, f.Tag.Get("json"))
			}
			walk(f.Type, p)
		}
	}
	walk(reflect.TypeFor[runner.ProxyConfig](), "ProxyConfig")
}
