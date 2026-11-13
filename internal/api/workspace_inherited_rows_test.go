// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestWorkspaceOwnerReadsInheritedSourceRowsAsAMember: a member who owns a workspace attaching a source an
// operator annotated reads the inherited secret and internal-host rows as a member reading the operator's
// workspace does, on the list and the detail read, while the rows the workspace itself holds and the rows a
// scan found stay. A super admin still reads every row.
func TestWorkspaceOwnerReadsInheritedSourceRowsAsAMember(t *testing.T) {
	const owner = "sub-inheriting-owner"
	const inheritedSecret, inheritedHost, scanFound = "corp-inherited-db", "inherited.corp.internal", "scan-found-key"
	for _, c := range []struct {
		name      string
		session   *http.Cookie
		forbidden []string
	}{
		{"the workspace's owner", ssoSession(t, owner, "owner@corp.example", oidc.RoleUser), []string{inheritedSecret, inheritedHost}},
		{"a super admin", ssoSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, st := newTopologyWorkspaceServer(t, owner)
			eff := maps.Clone(st.ws.Requirements)
			eff["secret:"+inheritedSecret] = types.WorkspaceRequirement{Level: "required", Provenance: "operator_set"}
			eff["egress:"+inheritedHost] = types.WorkspaceRequirement{Level: "required", Provenance: "operator_set"}
			eff["secret:"+scanFound] = types.WorkspaceRequirement{Level: "optional", Provenance: "scan_seeded"}
			st.ws.EffectiveRequirements = eff
			for _, path := range []string{"/api/v1/workspaces", "/api/v1/workspaces/" + st.ws.ID.String()} {
				w := doSSO(t, srv, http.MethodGet, path, c.session, "")
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s = %d, want 200; body=%s", path, w.Code, w.Body.String())
				}
				body := w.Body.String()
				for _, bad := range c.forbidden {
					if strings.Contains(body, bad) {
						t.Errorf("GET %s showed the inherited row %q to the owner.\nbody=%s", path, bad, body)
					}
				}
				want := []string{r3TopologySecret, scanFound}
				if c.forbidden == nil {
					want = append(want, inheritedSecret, inheritedHost)
				}
				for _, s := range want {
					if !strings.Contains(body, s) {
						t.Errorf("GET %s dropped %q.\nbody=%s", path, s, body)
					}
				}
			}
		})
	}
}

// TestWithholdInheritedRowsSites: every workspace document a request returns to its owner goes through the
// inherited-row projection: the list, the detail read, the write echo, the create and update responses, and
// the setup checklist's own-workspace rows.
func TestWithholdInheritedRowsSites(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]int{"withholdInheritedRows": {}, "withholdInheritedRequirements": {}}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch f := call.Fun.(type) {
				case *ast.Ident:
					name = f.Name
				case *ast.SelectorExpr:
					name = f.Sel.Name
				}
				if m, ok := got[name]; ok {
					m[fn.Name.Name]++
				}
				return true
			})
		}
	}
	want := map[string]map[string]int{
		"withholdInheritedRows": {"handleListWorkspaces": 1, "workspaceForResponse": 1, "workspaceWriteEcho": 1,
			"handleCreateWorkspace": 1, "handleUpdateWorkspace": 1},
		"withholdInheritedRequirements": {"withholdInheritedRows": 1, "setupMemberView": 1},
	}
	for name, sites := range want {
		if !maps.Equal(got[name], sites) {
			t.Errorf("%s is called from %v, want %v", name, got[name], sites)
		}
	}
}
