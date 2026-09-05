// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// response_parity_test.go pins the SDK's RESPONSE DTOs against the server
// structs that produce them.
//
// Request bodies need no such test: internal/api declares each one as
// `type xRequest = client.XRequest` and pins those aliases at COMPILE time
// (internal/api/dto_alias_test.go), so a re-expanded copy fails to build. A
// RESPONSE cannot get that treatment from here — the server's own types are
// unexported — and so it had nothing at all: client.RunFiles and
// internal/api's runFilesResponse are two independent structs describing one
// wire contract, and renaming a json tag on the server left every test in the
// tree green while every SDK caller silently read a zero value.
//
// This parses the server's declaration and compares the json tags, INCLUDING
// their options: `omitempty` on a field the client expects always-present is
// the same class of drift.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// serverJSONTags returns the json struct tags of typeName as declared in the
// repo-relative Go file, in declaration order.
func serverJSONTags(t *testing.T, relPath, typeName string) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	// Guard the guard: a wrong root would silently parse nothing and pass.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	path := filepath.Join(root, relPath)
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", relPath, err)
	}

	var tags []string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		found = true
		for _, fld := range st.Fields.List {
			if fld.Tag == nil {
				tags = append(tags, "<no tag>")
				continue
			}
			raw, uerr := strconv.Unquote(fld.Tag.Value)
			if uerr != nil {
				t.Fatalf("%s.%v: unquote tag %s: %v", typeName, fld.Names, fld.Tag.Value, uerr)
			}
			tags = append(tags, reflect.StructTag(raw).Get("json"))
		}
		return false
	})
	if !found {
		t.Fatalf("%s declares no struct type %s — the server DTO moved or was renamed, and this guard can no longer see the contract it exists to check", relPath, typeName)
	}
	if len(tags) < 3 {
		t.Fatalf("%s.%s parsed %d fields (%v) — the parse regressed and this guard would pass vacuously", relPath, typeName, len(tags), tags)
	}
	return tags
}

// clientJSONTags returns v's json struct tags in declaration order.
func clientJSONTags(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	tags := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tags = append(tags, rt.Field(i).Tag.Get("json"))
	}
	return tags
}

func TestResponseDTOs_MatchTheServersWireTags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     string
		server   string
		clientDT any
	}{
		{"RunFiles", "internal/api/run_files.go", "runFilesResponse", client.RunFiles{}},
		{"RunFileStat", "internal/api/run_files.go", "runFileStat", client.RunFileStat{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := serverJSONTags(t, tc.file, tc.server)
			got := clientJSONTags(t, tc.clientDT)
			// Field ORDER is not part of the wire contract; the tag set is.
			slices.Sort(want)
			slices.Sort(got)
			if !slices.Equal(want, got) {
				t.Errorf("client.%s json tags %v do not match %s's %s %v\n"+
					"They are two independent structs describing ONE response body, so a rename or an added/dropped `omitempty` on either side is a silent break: the SDK decodes a zero value and every caller reads it as an honest answer.\n"+
					"Fix the side that moved, or give the server the alias treatment its REQUEST bodies get (internal/api/dto_alias_test.go).",
					tc.name, got, tc.file, tc.server, want)
			}
			if strings.Contains(strings.Join(got, ","), "<no tag>") {
				t.Errorf("client.%s has an untagged field: %v", tc.name, got)
			}
		})
	}
}
