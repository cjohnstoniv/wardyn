// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitremote

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// resolveConfigPath maps a .git entry to the config file to read: a directory
// to its own config, a "gitdir:" pointer to the target's config only while the
// target stays inside the workspace root, and anything else to "" (skipped).
func TestResolveConfigPath(t *testing.T) {
	root := ResolveRoot(t.TempDir())
	outside := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	resolve := func(p string) string {
		t.Helper()
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		fi, err := fs.Stat(foldersync.FS(dir), rel)
		if err != nil {
			t.Fatal(err)
		}
		return resolveConfigPath(dir, rel, fi)
	}

	gitDir := filepath.Join(root, "repo", ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := resolve(gitDir), "repo/.git/config"; got != want {
		t.Errorf("directory = %q, want %q", got, want)
	}

	abs := filepath.Join(root, "modules", "abs")
	for _, d := range []string{filepath.Join(root, "modules", "rel"), abs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "modules", "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for name, tc := range map[string]struct {
		content string
		want    string
	}{
		"relative pointer":          {"gitdir: modules/rel\n", "modules/rel/config"},
		"absolute pointer inside":   {"gitdir: " + abs + "\n", "modules/abs/config"},
		"pointer climbing out":      {"gitdir: ../../../etc\n", ""},
		"pointer through a link":    {"gitdir: modules/out\n", ""},
		"pointer to a missing dir":  {"gitdir: modules/missing\n", ""},
		"absolute pointer outside":  {"gitdir: " + outside + "\n", ""},
		"a file that is no pointer": {"[remote \"origin\"]\n", ""},
		"an empty file":             {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := resolve(write("dotgit-"+filepath.Base(name), tc.content)); got != tc.want {
				t.Errorf("resolveConfigPath = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("a symlinked entry is not followed", func(t *testing.T) {
		target := write("real-pointer", "gitdir: modules/rel\n")
		link := filepath.Join(root, "link-pointer")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if got := resolve(link); got != "" {
			t.Errorf("resolveConfigPath through a symlink = %q, want empty", got)
		}
	})
}
