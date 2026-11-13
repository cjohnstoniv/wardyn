// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit stands in for the sandbox's git: init makes .git, and the step named
// by FAIL_GIT fails after doing so.
const fakeGit = `#!/bin/sh
[ "$1" = init ] && mkdir -p .git
[ "$1" = "$FAIL_GIT" ] && { echo "git $1 failed" >&2; exit 1; }
exit 0
`

// A send into a directory with no repository makes one in four steps. A step
// that fails must fail the send, print no "fresh" and leave no .git, so the
// next send starts fresh again; a failed fetch into an existing repository
// fails the send and leaves that repository alone.
func TestSendGitScript_AFailedStepFailsTheSend(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, c := range []struct {
		name, fail string
		existing   bool
	}{
		{"init fails", "init", false}, {"fetch fails", "fetch", false}, {"symbolic-ref fails", "symbolic-ref", false},
		{"reset fails", "reset", false}, {"nothing fails", "", false}, {"fetch into an existing repository fails", "fetch", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.existing {
				if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("FAIL_GIT", c.fail)
			out, err := exec.Command("sh", "-c", sendGitScript(dir, filepath.Join(dir, "b.bundle"), "feature/x")).Output()
			_, statErr := os.Stat(filepath.Join(dir, ".git"))
			switch {
			case c.fail == "":
				if err != nil || strings.TrimSpace(string(out)) != "fresh" || statErr != nil {
					t.Fatalf("clean send: out %q, err %v, .git %v; want fresh and a repository", out, err, statErr)
				}
			case err == nil || strings.Contains(string(out), "fresh") || strings.Contains(string(out), "existing"):
				t.Errorf("a failed %s: out %q, err %v; want the send to fail", c.fail, out, err)
			case c.existing && statErr != nil:
				t.Errorf("a failed fetch removed the repository that was already there: %v", statErr)
			case !c.existing && statErr == nil:
				t.Errorf("a failed %s left a .git behind, so the next send would find a repository with no branch", c.fail)
			}
		})
	}
}
