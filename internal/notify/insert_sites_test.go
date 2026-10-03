// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestApprovalInsertSites_AreExactlyTheTwoThatEnqueue is the source guard: a non-test `INSERT INTO
// approvals` anywhere but the two seams that write outbox rows beside it would create approvals nobody
// is told about. A third site must call notify.NewEnqueue before it is added here.
func TestApprovalInsertSites_AreExactlyTheTwoThatEnqueue(t *testing.T) {
	insert := regexp.MustCompile(`(?i)INSERT\s+INTO\s+approvals\b`)
	var sites []string
	for _, root := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join("..", "..", root), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for range insert.FindAllIndex(src, -1) {
				sites = append(sites, filepath.ToSlash(strings.TrimPrefix(path, filepath.Join("..", ".."))))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(sites)
	want := []string{"/internal/broker/sql.go", "/internal/store/store.go"}
	if !slices.Equal(sites, want) {
		t.Fatalf("INSERT INTO approvals appears at %v, want exactly %v", sites, want)
	}
}
