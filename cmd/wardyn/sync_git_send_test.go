// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// A --git into a sandbox directory with no repository whose fetch fails must
// fail, record nothing as sent, say so, and keep the .git it was setting up;
// the next --git then finishes the repository and checks the branch out.
func TestSyncGateway_FailedFreshGitSendIsNotRecordedAsSent(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	t.Setenv("HOME", t.TempDir())
	for _, kv := range [][2]string{{"GIT_AUTHOR_NAME", "Dev"}, {"GIT_AUTHOR_EMAIL", "dev@example.com"}, {"GIT_COMMITTER_NAME", "Dev"}, {"GIT_COMMITTER_EMAIL", "dev@example.com"}} {
		t.Setenv(kv[0], kv[1])
	}
	// The sandbox's git is the host's here, so a wrapper refuses fetch while the marker exists.
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "refuse-fetch")
	putSyncFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = fetch ] && [ -e '"+marker+"' ]; then echo 'fetch refused' >&2; exit 1; fi\nexec '"+realGit+"' \"$@\"\n", 0o755)
	putSyncFile(t, marker, "", 0o644)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, false)
	gitIn(t, root, "init", "-q", "-b", "feature/x")
	putSyncFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	gitIn(t, root, "add", "main.go")
	gitIn(t, root, "commit", "-q", "-m", "first")
	mustPass(t, sy)

	ctx := context.Background()
	c := g.dial()
	var out strings.Builder
	if err := foldersync.SendGit(ctx, &out, sy, c, g.runID, root); err == nil || !strings.Contains(err.Error(), "fetch refused") ||
		!strings.Contains(err.Error(), "it is kept, and the next --git carries on with it") {
		t.Fatalf("a --git whose fetch failed: err %v, out %q; want the sandbox's error and what is kept", err, out.String())
	}
	if sy.State.GitSent != nil {
		t.Errorf("a failed --git was recorded as sent: %+v", *sy.State.GitSent)
	}
	if _, err := os.Stat(filepath.Join(box, ".git", "wardyn-setup-incomplete")); err != nil {
		t.Errorf("a failed --git did not keep the .git it was setting up: %v", err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := foldersync.SendGit(ctx, &out, sy, c, g.runID, root); err != nil || !strings.Contains(out.String(), "a new repository") {
		t.Fatalf("the retry: err %v, out %q; want a new repository", err, out.String())
	}
	if got := gitIn(t, box, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/x" {
		t.Errorf("sandbox branch after the retry = %q, want feature/x", got)
	}
}
