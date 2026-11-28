// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestHashSaltOnlyInAllowedFiles walks every non-test Go file under internal and cmd. The row hash salt and the
// claim salt are named, as an identifier or in a string literal (a column, a JSON key, a setting), only in the
// files that hold or verify a row's hash. Every other reader names its columns and so cannot select a salt.
func TestHashSaltOnlyInAllowedFiles(t *testing.T) {
	allowed := map[string]bool{
		"internal/auditreceipt/rowhash.go":       true,
		"internal/auditreceipt/verify.go":        true,
		"internal/store/auditpartition.go":       true,
		"internal/store/auditchain.go":           true,
		"internal/store/auditchain_range.go":     true,
		"internal/store/auditcheckpoint.go":      true,
		"internal/store/store_devices.go":        true,
		"internal/types/device.go":               true,
		"internal/api/audit_partition_export.go": true,
		"internal/api/audit_receipt.go":          true,
	}
	idents := map[string]bool{"HashSalt": true, "ClaimSalt": true}
	literals := []string{"hash_salt", "claim_salt", "audit_claim_salt"}
	root := filepath.Join("..", "..")
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Errorf("parse %s: %v", rel, err)
				return nil
			}
			scanned++
			if allowed[rel] {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if idents[x.Name] {
						t.Errorf("%s names %s; only the files that hold or verify a row hash may", rel, x.Name)
					}
				case *ast.BasicLit:
					if x.Kind != token.STRING {
						break
					}
					for _, l := range literals {
						if strings.Contains(x.Value, l) {
							t.Errorf("%s has a literal naming %s; only the files that hold or verify a row hash may", rel, l)
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 500 {
		t.Fatalf("scanned %d files; the walk is looking at the wrong tree", scanned)
	}
}
