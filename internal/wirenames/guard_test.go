// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package wirenames

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// Governance profiles are units and governance assignments are memberships on every wire
// (DESIGN-TREE section 6). This guard fails when a Go json tag, a Go map-literal key (the audit
// payload and change-state shape) or a TypeScript wire key under ui/src/app/lib is spelled
// "profile(s)" or "assignment(s)" as a whole word of a snake_case name, unless the name belongs to
// another concept and is listed below with a reason.

// wireWord matches a snake_case name with a whole segment profile(s) or assignment(s).
var wireWord = regexp.MustCompile(`(^|_)(profiles?|assignments?)(_|$)`)

// otherConcept lists the "file:name" pairs (or "*:name" for any file) the rule does not apply to.
var otherConcept = map[string]string{
	// Another concept, wherever it appears ("*" matches any file).
	"*:default_profile":                                            "the ADO capability list a run gets by default",
	"*:built_profile_hash":                                         "the workspace scan profile hash",
	"*:key_domain_assignment":                                      "key-domain assignment",
	"*:key_domain_assignments":                                     "key-domain assignments",
	"*:assignments_deleted":                                        "key-domain assignments erased by a purge",
	"*:shell_profile":                                              "the setup check for the user's shell profile",
	"*:profile_proposal":                                           "the workspace profile proposal",
	"*:queue_profile_changed":                                      "HELD: a stored run end reason, see lanes/RENAME/TRIAGE.md",
	"internal/types/workspace.go:profile":                          "the workspace scan profile",
	"internal/types/workspace_contract.go:profile":                 "the workspace scan profile",
	"internal/adoscope/areas.go:profile":                           "the Azure DevOps user-profile scope area",
	"internal/api/key_domains.go:assignments":                      "key-domain assignments",
	"internal/api/governance_change_keydomain_kinds.go:assignment": "key-domain assignment",
	// Old spellings kept on purpose, to read rows or refuse documents written before 0.10.0.
	"internal/api/legacy_audit.go:profile":                  "reads audit rows written before 0.10.0, which are immutable",
	"cmd/wardyn/governance_renamed_keys.go:profiles":        "the 0.9 governance document keys, named in the refusal hint",
	"cmd/wardyn/governance_renamed_keys.go:assignments":     "the 0.9 governance document keys, named in the refusal hint",
	"cmd/wardyn/governance_renamed_keys.go:profile_id":      "the 0.9 governance document keys, named in the refusal hint",
	"cmd/wardyn/governance_renamed_keys.go:base_profile_id": "the 0.9 governance document keys, named in the refusal hint",
	"cmd/wardyn/governance_renamed_keys.go:profile":         "the 0.9 governance document keys, named in the refusal hint",
}

type finding struct{ File, Name, Kind string }

// scan walks the Go sources of internal, pkg and cmd and the TypeScript of ui/src/app/lib in fsys.
func scan(fsys fs.FS) ([]finding, error) {
	var out []finding
	for _, root := range []string{"internal", "pkg", "cmd", "ui/src/app/lib"} {
		err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				if path.Base(p) == "node_modules" || path.Base(p) == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			src, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			switch {
			case strings.HasSuffix(p, "_test.go"), strings.HasSuffix(p, ".test.ts"), strings.HasSuffix(p, ".test.tsx"):
				// tests spell what they assert; the guard is about the shipped wire
			case strings.HasSuffix(p, ".go"):
				fs, err := scanGo(p, src)
				if err != nil {
					return err
				}
				out = append(out, fs...)
			case strings.HasSuffix(p, ".ts"), strings.HasSuffix(p, ".tsx"):
				out = append(out, scanTS(p, src)...)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var kept []finding
	for _, f := range out {
		_, here := otherConcept[f.File+":"+f.Name]
		_, anywhere := otherConcept["*:"+f.Name]
		if !here && !anywhere {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

func scanGo(file string, src []byte) ([]finding, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []finding
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Field:
			if n.Tag == nil {
				return true
			}
			tag, _ := strconv.Unquote(n.Tag.Value)
			name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
			if wireWord.MatchString(name) {
				out = append(out, finding{file, name, "json tag"})
			}
		case *ast.CompositeLit:
			if _, ok := n.Type.(*ast.MapType); !ok {
				return true
			}
			for _, el := range n.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if name, _ := strconv.Unquote(lit.Value); wireWord.MatchString(name) {
						out = append(out, finding{file, name, "map key"})
					}
				}
			}
		}
		return true
	})
	return out, nil
}

var snakeToken = regexp.MustCompile(`[a-z][a-z0-9]*(?:_[a-z0-9]+)+`)

func scanTS(file string, src []byte) []finding {
	var out []finding
	seen := map[string]bool{}
	for _, tok := range snakeToken.FindAllString(string(src), -1) {
		if wireWord.MatchString(tok) && !seen[tok] {
			seen[tok] = true
			out = append(out, finding{file, tok, "ts key"})
		}
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	if r := os.Getenv("WIRENAMES_ROOT"); r != "" {
		return r
	}
	return "../.."
}

func TestNoProfileWireNames(t *testing.T) {
	got, err := scan(os.DirFS(repoRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Errorf("%s: %s %q is spelled with profile or assignment; the wire word is unit or membership "+
			"(list it in otherConcept only if it names a different concept)", f.File, f.Kind, f.Name)
	}
}

// TestNoProfileWireNamesSeesAPlantedTwin proves the scanner reports each shape it claims to: a
// json tag, a map key and a TypeScript key, and ignores a listed other concept.
func TestNoProfileWireNamesSeesAPlantedTwin(t *testing.T) {
	fsys := fstest.MapFS{
		"internal/x/x.go":             {Data: []byte("package x\n\ntype T struct {\n\tID string `json:\"profile_id\"`\n\tOK string `json:\"unit_id\"`\n}\n\nvar m = map[string]any{\"base_profile_id\": 1, \"parent_id\": 2}\n")},
		"ui/src/app/lib/x.ts":         {Data: []byte("export interface X { profile_name: string; unit_name: string }\n")},
		"internal/types/workspace.go": {Data: []byte("package types\n\ntype W struct {\n\tProfile string `json:\"profile\"`\n}\n")},
	}
	got, err := scan(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.File+":"+f.Name)
	}
	slices.Sort(names)
	want := []string{"internal/x/x.go:base_profile_id", "internal/x/x.go:profile_id", "ui/src/app/lib/x.ts:profile_name"}
	if !slices.Equal(names, want) {
		t.Errorf("findings = %v, want %v", names, want)
	}
}
