// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// dto_parity_test.go pins the create-run wire DTO against the server's
// declaration STRUCTURALLY, so drift fails the build instead of being noticed
// later by a human reading two files side by side.
//
// Background: the POST /api/v1/runs body used to be declared THREE times —
// internal/api.createRunRequest (the server truth), pkg/client.CreateRunRequest
// (this SDK), and cmd/wardyn.createRunBody (the CLI) — and they had already
// diverged (the SDK was missing compose_session_id; the CLI was missing that
// plus devcontainer_repo/devcontainer_ref). The CLI declaration is gone: cmd/wardyn
// now posts pkg/client.CreateRunRequest itself, so CLI drift is a compile error.
// That leaves TWO declarations, and this test is what keeps them equal.
//
// It cannot use reflection on both sides: internal/api.createRunRequest is
// unexported, so no test outside that package can name it. Instead it parses
// internal/api's source with go/ast and compares the two json tag NAME sets.
// If internal/api ever exports the DTO (or, better, aliases this one — see the
// note on TestCreateRunRequest_MatchesServerDTO), this can collapse to a plain
// reflect comparison or disappear entirely.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	// serverPkgDir is the api package holding the authoritative DTO. Relative
	// paths are stable under `go test`: the test binary runs with its own
	// package directory as the working directory.
	serverPkgDir = "../../internal/api"
	// serverDTOName is the server's create-run body type.
	serverDTOName = "createRunRequest"
)

// jsonName returns the wire name a struct field marshals to, or "" when the
// field is skipped (`json:"-"`). It deliberately ignores tag OPTIONS
// (omitempty et al): options only affect marshaling, and the server DTO is
// decode-only, so `json:"task"` there and `json:"task,omitempty"` here are the
// same wire contract. Options are pinned separately by
// TestCreateRunRequest_ZeroValueOmitsOptionals.
func jsonName(tag, goName string) string {
	name, _, _ := strings.Cut(tag, ",")
	switch name {
	case "":
		return goName // untagged fields marshal under their Go name
	case "-":
		if !strings.Contains(tag, ",") {
			return "" // `json:"-"` skips; `json:"-,"` means a literal "-"
		}
		return "-"
	}
	return name
}

// sdkJSONNames reflects the wire name set off the SDK's exported DTO.
func sdkJSONNames(t *testing.T) []string {
	t.Helper()
	rt := reflect.TypeOf(client.CreateRunRequest{})
	var names []string
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if n := jsonName(f.Tag.Get("json"), f.Name); n != "" {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// serverJSONNames parses internal/api and returns the wire name set declared by
// serverDTOName. It fails loudly rather than silently passing if the type moved
// or was renamed — a silent skip is exactly the failure mode this test exists to
// prevent.
func serverJSONNames(t *testing.T) []string {
	t.Helper()
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
		f, err := parser.ParseFile(fset, filepath.Join(serverPkgDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		st := findStruct(f, serverDTOName)
		if st == nil {
			continue
		}
		return structJSONNames(t, st)
	}
	t.Fatalf("server DTO %s.%s not found under %s — if it was renamed or moved, "+
		"update serverPkgDir/serverDTOName here; do NOT delete this test, it is the "+
		"only thing keeping the SDK DTO in sync with the wire contract",
		filepath.Base(serverPkgDir), serverDTOName, serverPkgDir)
	return nil
}

// findStruct returns the struct literal declared as `type <name> struct{...}`.
func findStruct(f *ast.File, name string) *ast.StructType {
	var found *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != name {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			found = st
			return false
		}
		return true
	})
	return found
}

// structJSONNames pulls the wire name set off a parsed struct.
func structJSONNames(t *testing.T, st *ast.StructType) []string {
	t.Helper()
	var names []string
	for _, f := range st.Fields.List {
		var tag string
		if f.Tag != nil {
			lit, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				t.Fatalf("unquote struct tag %s: %v", f.Tag.Value, err)
			}
			tag = reflect.StructTag(lit).Get("json")
		}
		if len(f.Names) == 0 {
			// An embedded field flattens its own fields onto the wire; this
			// comparison cannot see through that, so refuse to guess.
			t.Fatalf("%s embeds a field — this parity test only understands named "+
				"fields; teach it to flatten embeds before adding one", serverDTOName)
		}
		for _, id := range f.Names {
			if !id.IsExported() {
				continue // unexported fields never reach the wire
			}
			if n := jsonName(tag, id.Name); n != "" {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return names
}

// TestCreateRunRequest_MatchesServerDTO fails the moment internal/api's
// createRunRequest and pkg/client.CreateRunRequest describe different wire
// bodies — in EITHER direction. A server field the SDK lacks is an SDK that
// cannot drive a supported feature (this is how compose_session_id,
// devcontainer_repo, and devcontainer_ref went missing); an SDK field the server
// lacks is an SDK that silently posts a no-op.
//
// This is a two-declaration workaround. The real single-sourcing needs a
// one-line change in internal/api (NOT owned by this package):
//
//	type createRunRequest = client.CreateRunRequest
//
// which makes the compiler the enforcement mechanism and lets this whole file be
// deleted. Until then, this test is the enforcement mechanism.
func TestCreateRunRequest_MatchesServerDTO(t *testing.T) {
	server := serverJSONNames(t)
	sdk := sdkJSONNames(t)

	if missing := missingFrom(sdk, server); len(missing) > 0 {
		t.Errorf("pkg/client.CreateRunRequest is MISSING server fields %v.\n"+
			"internal/api.createRunRequest accepts %v but the SDK only sends %v — "+
			"add the fields so SDK callers can drive them.", missing, server, sdk)
	}
	if extra := missingFrom(server, sdk); len(extra) > 0 {
		t.Errorf("pkg/client.CreateRunRequest declares fields %v that "+
			"internal/api.createRunRequest does not accept.\nSDK sends %v, server "+
			"reads %v — the extra fields are silently dropped by the server.", extra, sdk, server)
	}
}

// missingFrom returns the members of want that have absent.
func missingFrom(have, want []string) []string {
	var missing []string
	for _, w := range want {
		if !slices.Contains(have, w) {
			missing = append(missing, w)
		}
	}
	return missing
}

// TestCreateRunRequest_ZeroValueOmitsOptionals pins the tag OPTIONS that
// TestCreateRunRequest_MatchesServerDTO deliberately ignores: a minimal request
// must put only the two always-sent keys on the wire, so a new optional field
// added without omitempty (which would post e.g. "task_mode":"" and override a
// server default) fails here rather than in production.
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
