// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitremote

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// writeRepo creates <dir>/.git/config with the given remote URLs (origin, then
// extra remotes named r1, r2, ...).
func writeRepo(t *testing.T, dir string, urls ...string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, u := range urls {
		name := "origin"
		if i > 0 {
			name = "r" + string(rune('0'+i))
		}
		b.WriteString("[remote \"" + name + "\"]\n\turl = " + u + "\n")
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetect_TopLevelForms(t *testing.T) {
	cases := map[string]string{
		"https":       "https://github.com/acme/web.git",
		"https-noext": "https://github.com/acme/web",
		"scp":         "git@github.com:acme/web.git",
		"ssh":         "ssh://git@github.com/acme/web.git",
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeRepo(t, dir, url)
			gh, other := DetectGitHubRepos(dir)
			if !reflect.DeepEqual(gh, []string{"acme/web"}) {
				t.Errorf("github = %v, want [acme/web]", gh)
			}
			if len(other) != 0 {
				t.Errorf("other = %v, want none", other)
			}
		})
	}
}

func TestDetect_NoGitNoRepos(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gh, other := DetectGitHubRepos(dir)
	if gh != nil || other != nil {
		t.Errorf("a dir with no .git must yield no remotes, got gh=%v other=%v", gh, other)
	}
}

func TestDetect_NestedSubdirReposAndDedup(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "https://github.com/acme/web.git")
	writeRepo(t, filepath.Join(root, "services", "api"), "git@github.com:acme/api.git")
	// duplicate of the root remote in a sibling dir → deduped
	writeRepo(t, filepath.Join(root, "copy"), "https://github.com/acme/web")
	gh, _ := DetectGitHubRepos(root)
	want := []string{"acme/api", "acme/web"}
	if !reflect.DeepEqual(gh, want) {
		t.Errorf("github = %v, want %v", gh, want)
	}
}

func TestDetect_Gitmodules(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "https://github.com/acme/web.git")
	mods := "[submodule \"lib\"]\n\tpath = vendor/lib\n\turl = https://github.com/acme/lib.git\n"
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(mods), 0o644); err != nil {
		t.Fatal(err)
	}
	gh, _ := DetectGitHubRepos(root)
	want := []string{"acme/lib", "acme/web"}
	if !reflect.DeepEqual(gh, want) {
		t.Errorf("github = %v, want %v", gh, want)
	}
}

func TestDetect_NonGitHubRemoteIsOtherNotGitHub(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "git@gitlab.com:acme/internal.git", "https://bitbucket.org/acme/x.git")
	gh, other := DetectGitHubRepos(root)
	if len(gh) != 0 {
		t.Errorf("github = %v, want none", gh)
	}
	want := []string{"bitbucket.org", "gitlab.com"}
	if !reflect.DeepEqual(other, want) {
		t.Errorf("other = %v, want %v", other, want)
	}
}

func TestDetect_MixedGitHubAndOther(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "https://github.com/acme/web.git", "git@gitlab.com:acme/mirror.git")
	gh, other := DetectGitHubRepos(root)
	if !reflect.DeepEqual(gh, []string{"acme/web"}) {
		t.Errorf("github = %v, want [acme/web]", gh)
	}
	if !reflect.DeepEqual(other, []string{"gitlab.com"}) {
		t.Errorf("other = %v, want [gitlab.com]", other)
	}
}

func TestDetect_DoesNotFollowSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	writeRepo(t, root, "https://github.com/acme/web.git")
	// A repo OUTSIDE root that root symlinks to — must NOT be detected.
	outside := t.TempDir()
	writeRepo(t, outside, "https://github.com/evil/secret.git")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	gh, _ := DetectGitHubRepos(root)
	for _, r := range gh {
		if r == "evil/secret" {
			t.Fatalf("symlinked dir was followed: detected %v", gh)
		}
	}
	if !reflect.DeepEqual(gh, []string{"acme/web"}) {
		t.Errorf("github = %v, want [acme/web]", gh)
	}
}

func TestDetect_DepthCap(t *testing.T) {
	root := t.TempDir()
	// A repo deeper than maxDepth must be skipped.
	deep := root
	for i := 0; i < maxDepth+2; i++ {
		deep = filepath.Join(deep, "d")
	}
	writeRepo(t, deep, "https://github.com/acme/toodeep.git")
	gh, _ := DetectGitHubRepos(root)
	for _, r := range gh {
		if r == "acme/toodeep" {
			t.Errorf("repo below the depth cap should be skipped, got %v", gh)
		}
	}
}

func TestDetect_RejectsUnsafeURL(t *testing.T) {
	root := t.TempDir()
	// A URL with an embedded newline/space must be rejected by safe().
	writeRepo(t, root, "https://github.com/acme/ok")
	gitDir := filepath.Join(root, "evil", ".git")
	_ = os.MkdirAll(gitDir, 0o755)
	_ = os.WriteFile(filepath.Join(gitDir, "config"),
		[]byte("[remote \"o\"]\n\turl = https://github.com/a b/c\n"), 0o644)
	gh, _ := DetectGitHubRepos(root)
	if !reflect.DeepEqual(gh, []string{"acme/ok"}) {
		t.Errorf("github = %v, want only the safe [acme/ok]", gh)
	}
}
