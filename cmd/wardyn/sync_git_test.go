// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSyncGateway_GitHistoryCrossesAsABundleOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	t.Setenv("HOME", t.TempDir())
	for _, kv := range [][2]string{{"GIT_AUTHOR_NAME", "Dev"}, {"GIT_AUTHOR_EMAIL", "dev@example.com"}, {"GIT_COMMITTER_NAME", "Dev"}, {"GIT_COMMITTER_EMAIL", "dev@example.com"}} {
		t.Setenv(kv[0], kv[1])
	}
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, false)
	gitIn(t, root, "init", "-q", "-b", "feature/x")
	putSyncFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	gitIn(t, root, "add", "main.go")
	gitIn(t, root, "commit", "-q", "-m", "first")
	gitIn(t, root, "config", "core.sshCommand", "touch /tmp/should-never-run")
	putSyncFile(t, filepath.Join(root, ".git/hooks/post-checkout"), "#!/bin/sh\ntouch /tmp/should-never-run\n", 0o755)
	putSyncFile(t, filepath.Join(root, "main.go"), "package main // uncommitted\n", 0o644)
	mustPass(t, sy)

	ctx := context.Background()
	c := g.dial()
	var out strings.Builder
	if err := syncGitCheck(ctx, sy, root); err != nil {
		t.Fatal(err)
	}
	if err := sendGit(ctx, &out, sy, c, g.runID, root); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, box, "log", "--format=%s"); got != "first" {
		t.Errorf("git log in the sandbox = %q", got)
	}
	if got := gitIn(t, box, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/x" {
		t.Errorf("sandbox branch = %q", got)
	}
	if got := gitIn(t, box, "diff", "--name-only"); got != "main.go" {
		t.Errorf("git diff in the sandbox = %q, want the laptop's uncommitted change", got)
	}
	if _, err := os.Stat(filepath.Join(box, ".git/hooks")); err == nil {
		t.Error("the sandbox repository has a hooks directory")
	}
	if cfg := getSyncFile(t, filepath.Join(box, ".git/config")); strings.Contains(cfg, "sshCommand") {
		t.Errorf("the laptop's git config crossed: %s", cfg)
	}
	out.Reset()
	if err := sendGit(ctx, &out, sy, c, g.runID, root); err != nil || !strings.Contains(out.String(), "already") {
		t.Errorf("a second --git with nothing new: %v, %q", err, out.String())
	}

	// The agent commits; --pull-commits brings it back as a branch, never into the working tree.
	putSyncFile(t, filepath.Join(box, "agent.go"), "package main\n", 0o644)
	gitIn(t, box, "add", "agent.go")
	gitIn(t, box, "commit", "-q", "-m", "agent work")
	if err := pullCommits(ctx, &out, sy, c, g.runID, root); err != nil {
		t.Fatal(err)
	}
	branch := "wardyn/sync-" + g.runID[:8]
	if got := gitIn(t, root, "log", "--format=%s", branch); got != "agent work\nfirst" {
		t.Errorf("git log %s = %q", branch, got)
	}
	if got := gitIn(t, root, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/x" {
		t.Errorf("the laptop's checkout moved to %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "agent.go")); err == nil {
		t.Error("the sandbox's commit reached the laptop's working tree")
	}
	if _, err := os.Stat("/tmp/should-never-run"); err == nil {
		t.Error("a laptop hook or config command ran")
	}
}

func TestSyncGitCheck_NeedsTheTopOfAWorkTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	g := newSyncRig(t)
	g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, false)
	if err := syncGitCheck(context.Background(), sy, root); err == nil {
		t.Error("a directory outside any work tree was accepted")
	}
	gitIn(t, root, "init", "-q")
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syncGitCheck(context.Background(), sy, sub); err == nil {
		t.Error("a subdirectory of a work tree was accepted")
	}
	sy.o.remote = syncGitDir + "/x"
	if err := syncGitCheck(context.Background(), sy, root); err == nil {
		t.Error("a remote directory under the bundle directory was accepted")
	}
}

func TestPullCommitsFetch_ChecksObjectsAndSkipsTagsAndSubmodules(t *testing.T) {
	want := []string{"-c", "transfer.fsckObjects=true", "fetch", "-q", "--no-tags", "--no-recurse-submodules", "/tmp/b.bundle", "+HEAD:refs/heads/wardyn/sync-12345678"}
	if got := pullCommitsFetch("/tmp/b.bundle", "wardyn/sync-12345678"); !slices.Equal(got, want) {
		t.Errorf("fetch argv = %q, want %q", got, want)
	}
}
