// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSyncDenied(t *testing.T) {
	for _, rel := range []string{
		".git", ".git/hooks/pre-commit", "a/.git/config", ".GIT/config", ".envrc", "pkg/.envrc", ".ENVRC", ".direnv/x", "a/.direnv",
		".vscode/tasks.json", ".vscode/launch.json", "a/.vscode/tasks.json", ".VSCode/Tasks.json",
		".idea/runConfigurations/x.xml", ".idea/runConfigurations", ".idea/workspace.xml", "a/.idea/workspace.xml",
	} {
		if !syncDenied(rel) {
			t.Errorf("%q is not denied", rel)
		}
	}
	for _, rel := range []string{
		"", "main.go", ".gitignore", ".github/workflows/ci.yml", ".gitattributes", "envrc", "a.envrc", ".vscode/settings.json",
		".vscode", ".idea/misc.xml", ".idea", "src/.vscode-test/x", "git/config",
	} {
		if syncDenied(rel) {
			t.Errorf("%q is denied", rel)
		}
	}
}

func TestSyncRemoteDir(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/home/agent/work",
		"/home/agent/work/proj/": "/home/agent/work/proj",
		"/home/agent//x/./y":     "/home/agent/x/y",
	} {
		if got, err := syncRemoteDir(in); err != nil || got != want {
			t.Errorf("syncRemoteDir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"work", "./work", "/", "/etc", "/home/agent", "/home/agent/", "/home/agentx/w", "/home/agent/../etc",
		"/home/agent/work/..", "/home/agent/%d", "/home/agent/a\nb", "/home/agent/a\x7f", "/home/agent/\xff",
	} {
		if got, err := syncRemoteDir(in); err == nil {
			t.Errorf("syncRemoteDir(%q) = %q, want refused", in, got)
		}
	}
}

func TestSyncEntryName(t *testing.T) {
	for _, n := range []string{"a", "a b", ".hidden", "..x", "x.."} {
		if err := syncEntryName(n); err != nil {
			t.Errorf("%q refused: %v", n, err)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "\xff"} {
		if err := syncEntryName(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestSyncCollisions(t *testing.T) {
	got := syncCollisions([]string{"a/B.txt", "a/b.TXT", "c/d", "c/d", "e", "E", "E/x"})
	for _, p := range []string{"a/B.txt", "a/b.TXT", "e", "E"} {
		if !got[p] {
			t.Errorf("%q not flagged: %v", p, got)
		}
	}
	if !syncCollides(got, "E/x") {
		t.Error("a file under a colliding directory is not refused")
	}
	if got["c/d"] || len(got) != 4 || syncCollides(got, "c/d") {
		t.Errorf("one path listed twice is not a collision: %v", got)
	}
}

func TestSyncLocalPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	_ = os.Symlink(outside, filepath.Join(root, "link"))
	_ = os.MkdirAll(filepath.Join(root, "real"), 0o755)
	_ = os.Symlink(outside, filepath.Join(root, "real", "inner"))

	for _, rel := range []string{"a.txt", "real/x", "new/deep/x"} {
		if _, err := syncLocalPath(root, rel); err != nil {
			t.Errorf("%q refused: %v", rel, err)
		}
	}
	for _, rel := range []string{"", "..", "../x", "a/../../x", "/etc/passwd", "a//b", "./a", "a/./b", "link", "link/x", "real/inner/x", "a/..\x00"} {
		if got, err := syncLocalPath(root, rel); err == nil {
			t.Errorf("%q = %q, want refused", rel, got)
		}
	}
}

func TestSyncStateStaysOutsideTheTree(t *testing.T) {
	root := t.TempDir()
	if err := syncStateOutside(root, filepath.Join(root, "x", "state.json")); err == nil {
		t.Error("a state file inside the synced directory was accepted")
	}
	if err := syncStateOutside(root, filepath.Join(filepath.Dir(root), "elsewhere", "state.json")); err != nil {
		t.Errorf("a state file outside refused: %v", err)
	}
	t.Setenv("HOME", root)
	p, err := syncStatePath("run-1")
	if err != nil || !strings.HasSuffix(filepath.ToSlash(p), ".local/state/wardyn/sync/run-1.json") {
		t.Errorf("state path = %q, %v", p, err)
	}
	if err := syncStateOutside(root, p); err == nil {
		t.Error("syncing $HOME would put the state file in the tree")
	}
}

func TestSyncStateRoundTripAndPairing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "s", "state.json")
	st, err := loadSyncState(file, "/l", "/home/agent/r")
	if err != nil || len(st.Files) != 0 {
		t.Fatalf("fresh state: %v %+v", err, st)
	}
	st.Files["a"] = syncFileSeen{Local: syncSide{1, 2}, Remote: syncSide{3, 4}}
	if err := st.save(file); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode = %v", fi.Mode())
	}
	back, _ := loadSyncState(file, "/l", "/home/agent/r")
	if !slices.Equal(keysOf(back.Files), []string{"a"}) {
		t.Errorf("state not restored: %+v", back)
	}
	other, _ := loadSyncState(file, "/l2", "/home/agent/r")
	if len(other.Files) != 0 {
		t.Error("state for another directory pairing was reused")
	}
	_ = os.WriteFile(file, []byte("{"), 0o600)
	if _, err := loadSyncState(file, "/l", "/home/agent/r"); err == nil {
		t.Error("a corrupt state file was silently ignored")
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
