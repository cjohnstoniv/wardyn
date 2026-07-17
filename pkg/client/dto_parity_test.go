// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// dto_parity_test.go pins the create-run wire DTO to a SINGLE declaration.
//
// Background: the POST /api/v1/runs body used to be declared THREE times —
// internal/api.createRunRequest (the server truth), pkg/client.CreateRunRequest
// (this SDK), and cmd/wardyn.createRunBody (the CLI) — and they had already
// diverged (the SDK was missing compose_session_id; the CLI was missing that
// plus devcontainer_repo/devcontainer_ref). Both copies are now gone:
//
//   - cmd/wardyn posts pkg/client.CreateRunRequest directly (no CLI DTO).
//   - internal/api declares `type createRunRequest = client.CreateRunRequest`,
//     a TYPE ALIAS, so the server body IS this SDK type. Drift is a compile
//     error, and internal/api/dto_alias_test.go pins the identity at compile
//     time from inside that package.
//
// This test is the EXTERNAL guard: client_test cannot name the unexported
// createRunRequest, so it parses internal/api's source with go/ast and asserts
// the declaration is still that alias — never re-expanded into an independent
// struct that could silently drift again. Plus it pins the SDK's tag OPTIONS,
// which the alias identity does not cover.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	// serverPkgDir is the api package holding the authoritative alias. Relative
	// paths are stable under `go test`: the test binary runs with its own
	// package directory as the working directory.
	serverPkgDir = "../../internal/api"
	// serverDTOName is the server's create-run body type.
	serverDTOName = "createRunRequest"
)

// TestCreateRunRequest_AliasesSDKDTO fails the moment internal/api stops
// aliasing pkg/client.CreateRunRequest — i.e. re-expands createRunRequest into
// an independent struct, which is exactly how the CLI/SDK/server declarations
// drifted apart before (missing compose_session_id, devcontainer_repo,
// devcontainer_ref). While it stays an alias the compiler guarantees the SDK,
// the server handler, and the CLI all read/write the identical field set, so no
// per-field wire comparison is needed here; this test just guards the alias
// itself from an external vantage point.
func TestCreateRunRequest_AliasesSDKDTO(t *testing.T) {
	entries, err := os.ReadDir(serverPkgDir)
	if err != nil {
		t.Fatalf("read %s: %v", serverPkgDir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(serverPkgDir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		if ts := findTypeSpec(f, serverDTOName); ts != nil {
			assertAliasesClientDTO(t, ts)
			return
		}
	}
	t.Fatalf("server DTO %s.%s not found under %s — if it was renamed or moved, "+
		"update serverPkgDir/serverDTOName here; do NOT delete this test, it is the "+
		"external guard that the server body stays a SINGLE-SOURCED alias of the SDK DTO",
		filepath.Base(serverPkgDir), serverDTOName, serverPkgDir)
}

// findTypeSpec returns the `type <name> ...` declaration, alias or struct.
func findTypeSpec(f *ast.File, name string) *ast.TypeSpec {
	var found *ast.TypeSpec
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if ok && ts.Name.Name == name {
			found = ts
			return false
		}
		return true
	})
	return found
}

// assertAliasesClientDTO fails unless ts is `type createRunRequest =
// client.CreateRunRequest` (an alias — ts.Assign is set — to the SDK selector).
func assertAliasesClientDTO(t *testing.T, ts *ast.TypeSpec) {
	t.Helper()
	if !ts.Assign.IsValid() {
		t.Fatalf("%s is declared as a defined type (struct copy), not a type alias — "+
			"restore `type %s = client.CreateRunRequest` so the server body stays "+
			"single-sourced with the SDK DTO and cannot drift", serverDTOName, serverDTOName)
	}
	sel, ok := ts.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "CreateRunRequest" {
		t.Fatalf("%s aliases %v, want client.CreateRunRequest", serverDTOName, ts.Type)
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "client" {
		t.Fatalf("%s aliases a CreateRunRequest from the wrong package %v, want client.CreateRunRequest", serverDTOName, sel.X)
	}
}

// TestCreateRunRequest_ZeroValueOmitsOptionals pins the tag OPTIONS the alias
// identity does not cover: a minimal request must put only the two always-sent
// keys on the wire, so a new optional field added without omitempty (which would
// post e.g. "task_mode":"" and override a server default) fails here rather than
// in production.
func TestCreateRunRequest_ZeroValueOmitsOptionals(t *testing.T) {
	b, err := json.Marshal(client.CreateRunRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := slices.Sorted(maps.Keys(raw))
	want := []string{"agent", "repo"}
	if !slices.Equal(got, want) {
		t.Errorf("zero-value CreateRunRequest marshals to keys %v, want %v — every "+
			"optional field needs `omitempty` so a minimal run does not post empty "+
			"values the server would treat as explicit choices", got, want)
	}
}
