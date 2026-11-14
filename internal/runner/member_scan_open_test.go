// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// A member's source, or a folder above it, swapped for a link to a folder outside their roots
// between the source check and its open is refused with the member refusal, and nothing outside is
// opened. With no swap the handle is the checked folder.
func TestOpenUserSource_ASourceOrAFolderAboveItSwappedForALinkAfterItsCheckIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	for name, tc := range map[string]struct {
		src, swap string // under the member root: the source, and the folder the swap replaces
	}{
		"the source itself":     {"a/app", "a/app"},
		"a folder above it":     {"a/app", "a"},
		"no swap (the control)": {"a/app", ""},
	} {
		t.Run(name, func(t *testing.T) {
			root, _ := memberRoot(t)
			outside := filepath.Join(filepath.Dir(root), "elsewhere")
			for base, token := range map[string]string{root: "INSIDE_TOKEN", outside: "OUTSIDE_TOKEN"} {
				if err := os.MkdirAll(filepath.Join(base, tc.src), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(base, tc.src, ".env.example"), []byte(token+"=\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.swap != "" {
				old := userSourceChecked
				userSourceChecked = func() {
					at := filepath.Join(root, tc.swap)
					if err := os.Rename(at, at+".old"); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(filepath.Join(outside, tc.swap), at); err != nil {
						t.Error(err)
					}
				}
				t.Cleanup(func() { userSourceChecked = old })
			}
			dir, err := UserMountPolicy{Roots: []string{root}}.OpenUserSource("alice", filepath.Join(root, tc.src), foldersync.OpenFromTop)
			if tc.swap != "" {
				if dir != nil {
					_ = dir.Close()
				}
				if !errors.Is(err, errMemberSourceRefused) || dir != nil || strings.Contains(err.Error(), outside) {
					t.Fatalf("OpenUserSource = %v, %v; want no handle and the member refusal, without the outside path", dir, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("OpenUserSource = %v, want the checked folder", err)
			}
			t.Cleanup(func() { _ = dir.Close() })
			f, err := dir.Open(".env.example")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if body, _ := io.ReadAll(f); string(body) != "INSIDE_TOKEN=\n" {
				t.Errorf("read %q through the handle, want the checked folder's file", body)
			}
		})
	}
}
