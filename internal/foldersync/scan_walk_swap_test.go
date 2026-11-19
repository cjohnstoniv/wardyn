// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync_test

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// A folder scan lists through foldersync.FS: a folder swapped for a link to another folder inside
// the scanned one, after its parent's listing showed a plain folder, is not listed, and the scan is
// incomplete. Listing through os.Root's own FS follows that link and reports the names behind it.
func TestCollectFactsIn_AFolderSwappedForALinkAfterItsListingIsNotListed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The walk lists the root (first, zz), reads first/Makefile, then lists zz. hidden is under a
	// folder name the walk skips, so its names are reached only through the link.
	for rel, body := range map[string]string{
		"first/Makefile":                   "build:\n",
		"zz/keep.txt":                      "x\n",
		"node_modules/hidden/.env":         "A=1\n",
		"node_modules/hidden/package.json": "{}\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	// The first folder opened below the root is first (to list it, or to read first/Makefile):
	// the root's listing has already shown zz as a plain folder, and zz is not yet listed.
	var once sync.Once
	restore := foldersync.SwapChildLooked(func() {
		once.Do(func() {
			zz := filepath.Join(root, "zz")
			if err := os.Rename(zz, zz+".old"); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(filepath.Join("node_modules", "hidden"), zz); err != nil {
				t.Error(err)
			}
		})
	})
	defer restore()

	facts := workspacescan.CollectFactsIn(dir)
	if slices.Contains(facts.SecretFilesPresent, "zz/.env") {
		t.Errorf("secret files = %v: the scan listed a folder through a link", facts.SecretFilesPresent)
	}
	for _, m := range facts.ManifestsFound {
		if m.Path == "zz/package.json" {
			t.Errorf("manifests = %v: the scan listed a folder through a link", facts.ManifestsFound)
		}
	}
	if !facts.Unread {
		t.Error("the scan could not list zz and still says it is complete")
	}
	if fi, err := os.Lstat(filepath.Join(root, "zz")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("zz was not swapped for a link during the scan (%v): the pin did not run its case", err)
	}
}
