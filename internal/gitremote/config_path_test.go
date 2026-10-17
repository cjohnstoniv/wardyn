// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitremote

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveConfigPath maps a .git entry to the config file to read: a directory
// to its own config, a "gitdir:" pointer to the target's config only while the
// target stays inside the workspace root, and anything else to "" (skipped).
func TestResolveConfigPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	resolve := func(p string) string {
		t.Helper()
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return resolveConfigPath(p, fi, root)
	}

	dir := filepath.Join(root, "repo", ".git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := resolve(dir), filepath.Join(dir, "config"); got != want {
		t.Errorf("directory = %q, want %q", got, want)
	}

	abs := filepath.Join(root, "modules", "abs")
	for name, tc := range map[string]struct {
		content string
		want    string
	}{
		"relative pointer":          {"gitdir: modules/rel\n", filepath.Join(root, "modules", "rel", "config")},
		"absolute pointer inside":   {"gitdir: " + abs + "\n", filepath.Join(abs, "config")},
		"pointer climbing out":      {"gitdir: ../../../etc\n", ""},
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

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		root, p string
		want    bool
	}{
		{"/w", "/w", true},
		{"/w", "/w/a/b", true},
		{"/w", "/w/..hidden", true},
		{"/w", "/x", false},
		{"/w", "/w/../x", false},
		{"/w", "/", false},
	} {
		if got := within(tc.root, tc.p); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.root, tc.p, got, tc.want)
		}
	}
}
