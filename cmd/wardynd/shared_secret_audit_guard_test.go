// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// What an organisation's shared secret is called is hidden from a run's owner
// where they are served the run's audit rows (internal/api's sharedRefs), and
// that rule recognises exactly two shapes: a JSON object marked "shared", and a
// JSON object that carries a secret-naming key beside a "grant_id". A row that
// named a shared grant's secret in any other shape — a flat "secret_name" with
// neither, a name one level away from its grant id — would be served whole.
//
// So every place the non-test tree writes one of the secret-naming keys into a
// map is enumerated here and must be one of:
//
//   - a grant's SCOPE (assigned to a variable whose name ends in "scope", or
//     written under a Scope field). A scope is stored on the grant row and
//     embedded in rows whole, and the component gate marks a shared one;
//   - beside "grant_id" or "shared" in the same literal — the two shapes the
//     read rule strips;
//   - declared below, with the reason that row can never name a shared grant's
//     secret, and how many such writes the function makes.
//
// A new write reddens this test until it is put in one of those shapes or
// declared, and a declaration for code that is gone reddens it too.
//
// ponytail: map literals and index writes only, classified by syntax. A struct
// with a secret-naming json tag marshalled into a row is not seen here, and a
// scope is recognised by its variable's name; a type-checked walk of what
// reaches recordAudit is the upgrade if either is ever abused.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretNamingKeys mirrors internal/api's memberSecretScopeKeys. Spelled here
// rather than imported: a guard that took its subject from the package under
// scrutiny would move with it.
var secretNamingKeys = map[string]bool{"secret_name": true, "key_secret_ref": true, "known_hosts_secret_ref": true}

// declaredSecretNameWrite is one function allowed to write a secret-naming key
// in neither shape: why, and how many such writes it makes.
type declaredSecretNameWrite struct {
	writes int
	why    string
}

var declaredSecretNameWrites = map[string]declaredSecretNameWrite{
	// Rows left as they are by decision: they name an operator secret that
	// reached a run's owner before components existed, and no component grant
	// is involved in either. Hiding those names is a separate call.
	"internal/api/artifact_redirect.go:Server.planArtifactRedirect":      {1, "run.artifact.redirect names the corporate mirror's token secret, which site config binds to the redirect; it is never a component's shared grant"},
	"internal/api/runs_create_requirements.go:Server.skipRequiredSecret": {1, "run.requirement.skip names the secret a workspace requirement asked for and the fold declined to grant; no grant exists, shared or otherwise"},
	// Grant kinds that cannot be shared: a component's shared secret is
	// delivered as a header only (types.validateComponentDelivery), and its
	// env and file deliveries are the launching person's own secret.
	"internal/api/runs_dispatch_secrets.go:Server.resolveEnvSecretGrants": {1, "run.env_secret.resolve names the secret an env_secret grant resolved; an env_secret grant is never shared"},
	"internal/api/runs_dispatch_files.go:Server.resolveFileSecretGrants":  {1, "run.file_secret.resolve names the secret a file_secret grant resolved; a file_secret grant is never shared"},
	// Not rows at all: broker.Minted.Metadata. The mint row is built from the
	// grant's own scope (mintEvent), withSecretScope copies secret_scope alone
	// out of this map, and the mint answer does not serialise it.
	"internal/broker/broker_mint_kinds.go:Broker.mintAPIKey": {1, "Minted.Metadata of an api_key mint"},
	"internal/broker/broker_mint_kinds.go:Broker.mintGitPAT": {1, "Minted.Metadata of a git_pat mint"},
	"internal/broker/broker_mint_kinds.go:Broker.mintSSHKey": {1, "Minted.Metadata of an ssh_key mint"},
}

// secretNameWrite is one write of a secret-naming key into a map.
type secretNameWrite struct {
	rel, fn string
	line    int
	scope   bool // a grant's scope
	marked  bool // beside grant_id or shared
}

func (w secretNameWrite) key() string { return w.rel + ":" + w.fn }

func TestEveryAuditRowNamesASecretInAShapeTheReadRuleStrips(t *testing.T) {
	writes := collectSecretNameWrites(t)
	var scopes, marked int
	undeclared := map[string]int{}
	for _, w := range writes {
		switch {
		case w.scope:
			scopes++
		case w.marked:
			marked++
		default:
			undeclared[w.key()]++
			if _, ok := declaredSecretNameWrites[w.key()]; !ok {
				t.Errorf("%s:%d (%s) writes a secret-naming key into a map that is neither a grant's scope nor "+
					"beside \"grant_id\" or \"shared\".\n\tIf this map can reach an audit row, a run's owner is served "+
					"it whole: the read rule (internal/api sharedRefs) strips a shared grant's secret name only from an "+
					"object marked \"shared\" or one that carries \"grant_id\" at the same level. Put \"grant_id\" beside "+
					"the name, or embed the grant's scope instead.\n\tIf it can never name a shared grant's secret, add "+
					"it to declaredSecretNameWrites with the reason.", w.rel, w.line, w.fn)
			}
		}
	}
	// The scan is only worth its verdict if it sees what is known to be there:
	// the grant scopes dispatch authors, and the dropped-injection row.
	if scopes == 0 || marked == 0 {
		t.Fatalf("the scan found %d grant scopes and %d writes beside grant_id; it is reading the wrong thing", scopes, marked)
	}
	for key, d := range declaredSecretNameWrites {
		if got := undeclared[key]; got != d.writes {
			t.Errorf("declaredSecretNameWrites says %s makes %d such write(s) (%s) and it makes %d: classify the new "+
				"one, or drop the entry for code that is gone", key, d.writes, d.why, got)
		}
	}
}

// collectSecretNameWrites parses every non-test .go file under internal/, cmd/
// and pkg/ and returns each map literal, and each index write, that sets a
// secret-naming key.
func collectSecretNameWrites(t *testing.T) []secretNameWrite {
	t.Helper()
	root := repoRoot(t)
	var out []secretNameWrite
	for _, top := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				name := ""
				if fn, ok := decl.(*ast.FuncDecl); ok {
					name = funcName(fn)
				}
				for _, w := range secretNameWritesIn(decl) {
					w.rel, w.fn, w.line = filepath.ToSlash(rel), name, fset.Position(token.Pos(w.line)).Line
					out = append(out, w)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	return out
}

// secretNameWritesIn is the writes inside one declaration. line holds the
// node's token.Pos until the caller resolves it.
func secretNameWritesIn(decl ast.Decl) []secretNameWrite {
	var out []secretNameWrite
	var stack []ast.Node
	ast.Inspect(decl, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		switch x := n.(type) {
		case *ast.CompositeLit:
			if keys := stringKeys(x); namesASecret(keys) {
				out = append(out, secretNameWrite{line: int(x.Pos()), scope: underScope(stack), marked: keys["grant_id"] || keys["shared"]})
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				if key, _ := stringLit(idx.Index); !secretNamingKeys[key] {
					continue
				}
				base, _ := idx.X.(*ast.Ident)
				out = append(out, secretNameWrite{line: int(idx.Pos()), scope: base != nil && scopeName(base.Name)})
			}
		}
		stack = append(stack, n)
		return true
	})
	return out
}

// stringKeys is the string-literal keys of a keyed composite literal: a map's.
func stringKeys(lit *ast.CompositeLit) map[string]bool {
	keys := map[string]bool{}
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if k, ok := stringLit(kv.Key); ok {
				keys[k] = true
			}
		}
	}
	return keys
}

func namesASecret(keys map[string]bool) bool {
	for k := range keys {
		if secretNamingKeys[k] {
			return true
		}
	}
	return false
}

func scopeName(name string) bool { return strings.HasSuffix(strings.ToLower(name), "scope") }

// underScope reports whether the node whose ancestors are stack is a grant's
// scope: the value of a Scope field, or of the nearest enclosing assignment or
// declaration to a variable named for a scope.
func underScope(stack []ast.Node) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		switch x := stack[i].(type) {
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && id.Name == "Scope" {
				return true
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && scopeName(id.Name) {
					return true
				}
			}
			return false
		case *ast.ValueSpec:
			for _, id := range x.Names {
				if scopeName(id.Name) {
					return true
				}
			}
			return false
		}
	}
	return false
}
