// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed final rename must not leave the store's own .tmp-cast-* file
// behind. The destination is occupied by a directory, so os.Rename fails
// deterministically (EISDIR / ENOTDIR) without needing a real disk fault.
func TestFSStore_SaveCastRenameFailureRemovesTemp(t *testing.T) {
	for _, tc := range []struct{ name, suffix string }{{"bare", ""}, {"named", "sess1"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s, err := NewFSStore(root)
			if err != nil {
				t.Fatal(err)
			}
			const runID = "11111111-1111-4111-8111-111111111111"
			dst := filepath.Join(root, CastKey(runID, tc.suffix)+".cast")
			if err := os.Mkdir(dst, 0o700); err != nil {
				t.Fatal(err)
			}
			err = s.SaveCastNamed(context.Background(), runID, tc.suffix, strings.NewReader("data"))
			if err == nil {
				t.Fatal("want a rename failure, got nil")
			}
			t.Logf("SaveCastNamed error: %v", err)
			ents, _ := os.ReadDir(root)
			var tmps []string
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".tmp-cast-") {
					tmps = append(tmps, e.Name())
				}
			}
			if len(tmps) != 0 {
				t.Fatalf("orphaned temp files after rename failure: %v", tmps)
			}
			if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
				t.Fatalf("destination changed: %v %v", fi, err)
			}
		})
	}
}
