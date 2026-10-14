// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMemberDriveOwnerNeverUpdated pins owner_subject as write-once: no SET
// list in the store's SQL names it, so no statement can move a drive to
// another person.
func TestMemberDriveOwnerNeverUpdated(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	setList := regexp.MustCompile(`(?is)\bSET\b(.*?)(?:\bWHERE\b|\bRETURNING\b|\bFROM\b|$)`)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sql, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", f, lit.Value, err)
			}
			for _, m := range setList.FindAllStringSubmatch(sql, -1) {
				checked++
				if strings.Contains(strings.ToLower(m[1]), "owner_subject") {
					t.Errorf("%s: a SET list writes owner_subject: %s", f, strings.Join(strings.Fields(m[0]), " "))
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("read no SET lists; the scan is broken")
	}
}

// TestMemberDriveNeedsOwner refuses an empty owner before any database work:
// an empty owner_subject is an admin row.
func TestMemberDriveNeedsOwner(t *testing.T) {
	var st store.PG // no pool: a refusal must come first
	ctx := context.Background()
	if _, err := st.CreateMemberDrive(ctx, types.UserDrive{Name: "notes"}, 5); err == nil {
		t.Error("CreateMemberDrive with no owner succeeded")
	}
	if _, err := st.ListMemberDrivesOf(ctx, ""); err == nil {
		t.Error("ListMemberDrivesOf(\"\") succeeded")
	}
}
