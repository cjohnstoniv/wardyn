// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/gitremote"
)

// A scan over an open folder reads that folder only: its path later pointed at another tree gives
// none of that tree's facts, and a folder inside it swapped for a link after the walk saw a plain
// folder is not read through, even when the link stays inside.
func TestCollectFactsIn_ReadsTheOpenedFolderThroughNoLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, outside := gitremote.ResolveRoot(t.TempDir()), gitremote.ResolveRoot(t.TempDir())
	for base, token := range map[string]string{root: "INSIDE_TOKEN", outside: "OUTSIDE_TOKEN"} {
		writeFile(t, base, ".env.example", token+"=\n")
		writeFile(t, base, "sub/.env.example", token+"_SUB=\n")
		writeFile(t, base, "repo/.git/config", "[remote \"origin\"]\n\turl = https://github.com/acme/"+token+".git\n")
	}
	writeFile(t, root, "other/.env.example", "OTHER_TOKEN=\n")
	swap := func(at, target string) {
		if err := os.Rename(at, at+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
	}
	open := func() *os.Root {
		dir, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dir.Close() })
		return dir
	}
	names := func(facts ScanFacts) map[string]bool {
		out := map[string]bool{}
		for _, n := range facts.SecretRequirements {
			out[n.Name] = true
		}
		return out
	}

	t.Run("the folder's own path pointed elsewhere", func(t *testing.T) {
		dir := open()
		swap(root, outside)
		t.Cleanup(func() { _ = os.Remove(root); _ = os.Rename(root+".old", root) })
		facts := CollectFactsIn(dir)
		got := names(facts)
		if !got["INSIDE_TOKEN"] || !got["INSIDE_TOKEN_SUB"] || got["OUTSIDE_TOKEN"] || got["OUTSIDE_TOKEN_SUB"] {
			t.Errorf("names = %v, want the opened folder's names and none of the other tree's", got)
		}
		if gh := facts.GitRemotes.GitHub; len(gh) != 1 || gh[0] != "acme/INSIDE_TOKEN" {
			t.Errorf("github = %v, want only the opened folder's acme/INSIDE_TOKEN", gh)
		}
	})
	t.Run("a folder inside swapped for a link after the walk saw it", func(t *testing.T) {
		dir := open()
		swap(filepath.Join(root, "sub"), "other") // relative: os.Root would follow it
		var facts ScanFacts
		detectDotenvTemplate(scanFile{dir, "sub/.env.example"}, &facts)
		if len(facts.SecretRequirements) != 0 || !facts.Unread {
			t.Errorf("read %v through a folder swapped for a link (unread %v), want nothing read and the scan incomplete",
				facts.SecretRequirements, facts.Unread)
		}
	})
}

// A folder the scan cannot open at all was not read: the profile is incomplete, as for an entry
// the walk could not read, so a source scan keeps what earlier scans found.
func TestCollectFacts_AFolderThatCannotBeOpenedIsIncomplete(t *testing.T) {
	got := DeriveProfile(CollectFacts(filepath.Join(t.TempDir(), "missing")))
	if !got.Incomplete || got.Confidence != ConfidenceLow {
		t.Errorf("incomplete %v, confidence %q; want true, low", got.Incomplete, got.Confidence)
	}
}
