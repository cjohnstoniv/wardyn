// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A replace ref or a graft in the laptop's checkout changes what git shows of its history, not what the
// bundle's pack carries, so the bundle is checked in a repository of its own, where neither exists.
func TestMakeCheckedBundle_ChecksTheBundleNotTheCheckoutsView(t *testing.T) {
	deny := []string{"*.pem"}
	// hiddenHistory adds old.pem in one commit and removes it in the next, and returns the head and the
	// two commits' parent.
	hiddenHistory := func(g *gitRig) (head, added, parent string) {
		g.write(g.lap, "old.pem", "synthetic\n")
		g.run(g.lap, "add", "old.pem")
		g.run(g.lap, "commit", "-q", "-m", "pem")
		g.run(g.lap, "rm", "-q", "old.pem")
		g.run(g.lap, "commit", "-q", "-m", "gone")
		return g.run(g.lap, "rev-parse", "HEAD"), g.run(g.lap, "rev-parse", "HEAD~1"), g.run(g.lap, "rev-parse", "HEAD~2")
	}
	listed := func(g *gitRig) string {
		return g.run(g.lap, "log", "--name-only", "--format=", "--diff-filter=A", "--root", "-m")
	}
	check := func(t *testing.T, g *gitRig, head string) {
		t.Helper()
		if strings.Contains(listed(g), "old.pem") {
			t.Fatal("the checkout still lists old.pem; the test proves nothing")
		}
		err := makeCheckedBundle(t.Context(), g.lap, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), deny)
		var denied *HistoryDeniedError
		if !errors.As(err, &denied) || denied.Path != "old.pem" {
			t.Fatalf("got %v; want a refusal naming old.pem", err)
		}
	}
	t.Run("a replace ref hides the commit that adds it", func(t *testing.T) {
		g := newGitRig(t)
		head, added, parent := hiddenHistory(g)
		stand := g.run(g.lap, "commit-tree", "-p", parent, "-m", "stand-in", parent+"^{tree}")
		g.run(g.lap, "replace", added, stand)
		check(t, g, head)
	})
	t.Run("a replace ref hides the tip's own tree", func(t *testing.T) {
		g := newGitRig(t)
		g.write(g.lap, "old.pem", "synthetic\n")
		g.run(g.lap, "add", "old.pem")
		g.run(g.lap, "commit", "-q", "-m", "pem")
		head := g.run(g.lap, "rev-parse", "HEAD")
		stand := g.run(g.lap, "commit-tree", "-p", "HEAD~1", "-m", "stand-in", "HEAD~1^{tree}")
		g.run(g.lap, "replace", head, stand)
		if strings.Contains(g.run(g.lap, "ls-tree", "-r", "--name-only", head), "old.pem") {
			t.Fatal("the checkout still lists old.pem; the test proves nothing")
		}
		err := makeCheckedBundle(t.Context(), g.lap, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), deny)
		var denied *HistoryDeniedError
		if !errors.As(err, &denied) || denied.Path != "old.pem" {
			t.Fatalf("got %v; want a refusal naming old.pem", err)
		}
	})
	t.Run("a graft over the history", func(t *testing.T) {
		g := newGitRig(t)
		head, _, parent := hiddenHistory(g)
		g.write(filepath.Join(g.lap, ".git", "info"), "grafts", head+" "+parent+"\n")
		if strings.Contains(listed(g), "old.pem") {
			t.Skip("this git no longer honours a graft file")
		}
		// The pack lacks the commit the graft cuts away, so the bundle cannot be read back to be checked.
		err := makeCheckedBundle(t.Context(), g.lap, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), deny)
		if err == nil || !strings.Contains(err.Error(), "could not be read back") || !strings.Contains(err.Error(), "git fetch:") {
			t.Fatalf("got %v; want a refusal saying the bundle could not be read back, by git fetch", err)
		}
	})
	t.Run("no replace ref or graft is a plain refusal of the same history", func(t *testing.T) {
		g := newGitRig(t)
		head, _, _ := hiddenHistory(g)
		err := makeCheckedBundle(t.Context(), g.lap, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), deny)
		var denied *HistoryDeniedError
		if !errors.As(err, &denied) || denied.Path != "old.pem" {
			t.Fatalf("got %v; want a refusal naming old.pem", err)
		}
	})
}

// The repository the bundle is checked in is removed whether the check passes, refuses or fails.
func TestMakeCheckedBundle_RemovesItsScanRepository(t *testing.T) {
	g := newGitRig(t)
	beside := t.TempDir()
	left := func() []string {
		m, _ := filepath.Glob(filepath.Join(beside, "wardyn-bundle-scan-*"))
		return m
	}
	bundle := func() string { return filepath.Join(beside, "b.bundle") }
	if err := makeCheckedBundle(t.Context(), g.lap, "feature/x", g.head, bundle(), []string{"*.pem"}); err != nil {
		t.Fatal(err)
	}
	g.write(g.lap, "old.pem", "synthetic\n")
	g.run(g.lap, "add", "old.pem")
	g.run(g.lap, "commit", "-q", "-m", "pem")
	if err := makeCheckedBundle(t.Context(), g.lap, "feature/x", g.run(g.lap, "rev-parse", "HEAD"), bundle(), []string{"*.pem"}); err == nil {
		t.Fatal("a denied history was sent")
	}
	if err := checkBundleAllowed(t.Context(), filepath.Join(beside, "missing.bundle"), "feature/x", g.head, []string{"*.pem"}); err == nil {
		t.Fatal("a missing bundle was checked")
	}
	if m := left(); len(m) != 0 {
		t.Fatalf("left behind: %v", m)
	}
}

// The history listing does not depend on any log or diff setting: a signed commit whose signature check
// prints text, a merge or root format, a rename detection or a submodule setting changes no path it names.
func TestCheckHistoryAllowed_SignedCommitsAndHostileConfig(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	g := newGitRig(t)
	key := filepath.Join(t.TempDir(), "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	signers := filepath.Join(t.TempDir(), "signers")
	g.write(filepath.Dir(signers), "signers", "* "+string(pub))
	g.run(g.lap, "update-index", "--add", "--cacheinfo", "160000,"+g.head+",module.pem")
	g.run(g.lap, "commit", "-q", "-m", "a submodule path")
	g.write(g.lap, "signed.pem", "synthetic\n")
	g.run(g.lap, "add", "signed.pem")
	g.run(g.lap, "-c", "gpg.format=ssh", "-c", "user.signingkey="+key+".pub", "commit", "-q", "-S", "-m", "signed")
	tip := g.run(g.lap, "rev-parse", "HEAD")
	for _, kv := range [][2]string{{"gpg.ssh.allowedSignersFile", signers}, {"log.showSignature", "true"}, {"log.diffMerges", "off"},
		{"log.showRoot", "false"}, {"diff.renames", "copies"}, {"diff.noprefix", "true"}, {"diff.relative", "true"}, {"diff.ignoreSubmodules", "all"}} {
		g.run(g.lap, "config", kv[0], kv[1])
	}
	global := filepath.Join(t.TempDir(), "global")
	g.write(filepath.Dir(global), "global", "[log]\n\tshowSignature = true\n[diff]\n\tignoreSubmodules = all\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if shown := g.run(g.lap, "log", "-n1", "--format=%G?"); shown == "N" {
		t.Fatal("the commit is not signed; the test proves nothing")
	}
	for _, path := range []string{"signed.pem", "module.pem", "main.go"} {
		err := checkHistoryAllowed(t.Context(), g.lap, "feature/x", tip, []string{path})
		var denied *HistoryDeniedError
		if !errors.As(err, &denied) || denied.Path != path {
			t.Errorf("deny %s: got %v; want a refusal naming it", path, err)
		}
	}
	if err := checkHistoryAllowed(t.Context(), g.lap, "feature/x", tip, []string{"*.nothing"}); err != nil {
		t.Errorf("no path matches, yet: %v", err)
	}
}

// A shallow clone's bundle is not a whole history, so it is refused with a plain message.
func TestMakeCheckedBundle_RefusesAShallowClone(t *testing.T) {
	g := newGitRig(t)
	g.write(g.lap, "next.txt", "synthetic\n")
	g.run(g.lap, "add", "next.txt")
	g.run(g.lap, "commit", "-q", "-m", "next")
	clone := filepath.Join(t.TempDir(), "clone")
	g.run(t.TempDir(), "clone", "-q", "--depth", "1", "--branch", "feature/x", "file://"+g.lap, clone)
	head := g.run(clone, "rev-parse", "HEAD")
	err := makeCheckedBundle(t.Context(), clone, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), []string{"*.pem"})
	if err == nil || !strings.Contains(err.Error(), "shallow clone") {
		t.Fatalf("got %v; want a refusal saying the repository is a shallow clone", err)
	}
}

// A branch that moves while its bundle is made is read and bundled again once; a second move is an error.
func TestBundleCurrentBranch_RetriesOnceWhenTheBranchMoves(t *testing.T) {
	deny := []string{"*.pem"}
	once := func(g *gitRig) string {
		mark := filepath.Join(t.TempDir(), "moved")
		return "[ -e " + shellQuote(mark) + " ] && exit 0\ntouch " + shellQuote(mark) + "\n" + g.commitFileScript("late.txt")
	}
	t.Run("one move", func(t *testing.T) {
		g := newGitRig(t)
		g.moveBranchAround("bundle create", false, once(g))
		bundle := filepath.Join(t.TempDir(), "b.bundle")
		branch, head, sent, err := bundleCurrentBranch(t.Context(), g.lap, bundle, nil, deny)
		now := g.run(g.lap, "rev-parse", "feature/x")
		if err != nil || sent || branch != "feature/x" || head != now || head == g.head {
			t.Fatalf("got %q %q %v %v; want feature/x at the moved head %s", branch, head, sent, err, now)
		}
		if got, want := g.run(g.lap, "bundle", "list-heads", bundle), now+" refs/heads/feature/x"; got != want {
			t.Fatalf("bundle heads = %q, want %q", got, want)
		}
	})
	t.Run("every try moves", func(t *testing.T) {
		g := newGitRig(t)
		g.moveBranchAround("bundle create", false, "cd "+shellQuote(g.lap)+" && date +%s%N > late.txt && "+shellQuote(g.git)+" add late.txt && "+shellQuote(g.git)+" commit -q -m late\n")
		_, _, _, err := bundleCurrentBranch(t.Context(), g.lap, filepath.Join(t.TempDir(), "b.bundle"), nil, deny)
		if err == nil || !strings.Contains(err.Error(), "moved") {
			t.Fatalf("got %v; want a refusal saying the branch moved", err)
		}
	})
	t.Run("already sent", func(t *testing.T) {
		g := newGitRig(t)
		bundle := filepath.Join(t.TempDir(), "b.bundle")
		_, _, sent, err := bundleCurrentBranch(t.Context(), g.lap, bundle, &GitSent{Branch: "feature/x", Commit: g.head}, deny)
		if err != nil || !sent {
			t.Fatalf("got sent=%v err=%v; want sent", sent, err)
		}
		if _, err := os.Stat(bundle); err == nil {
			t.Fatal("a bundle was made for a commit already sent")
		}
	})
}

// A SHA-256 repository's bundle is checked in a SHA-256 scanner, allowed or denied; a git without SHA-256
// skips the test.
func TestMakeCheckedBundle_ChecksASHA256Bundle(t *testing.T) {
	g := newGitRig(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "feature/x", "--object-format=sha256").CombinedOutput(); err != nil {
		t.Skipf("this git has no SHA-256 repositories: %s", out)
	}
	g.write(dir, "main.go", "package main\n")
	g.run(dir, "add", "main.go")
	g.run(dir, "commit", "-q", "-m", "root")
	head := g.run(dir, "rev-parse", "HEAD")
	if len(head) != 64 {
		t.Fatalf("head %q is not a SHA-256 id; the test proves nothing", head)
	}
	deny := []string{"*.pem"}
	if err := makeCheckedBundle(t.Context(), dir, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), deny); err != nil {
		t.Fatalf("an allowed SHA-256 history was refused: %v", err)
	}
	g.write(dir, "old.pem", "synthetic\n")
	g.run(dir, "add", "old.pem")
	g.run(dir, "commit", "-q", "-m", "pem")
	head = g.run(dir, "rev-parse", "HEAD")
	err := makeCheckedBundle(t.Context(), dir, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), deny)
	var denied *HistoryDeniedError
	if !errors.As(err, &denied) || denied.Path != "old.pem" {
		t.Fatalf("got %v; want a refusal naming old.pem", err)
	}
}

// A commit the history walk cannot read is an error, not a clean history: rev-list prints the commits it
// reached before it failed, and diff-tree reads those without complaint.
func TestCheckHistoryAllowed_RefusesAHistoryItCannotWalk(t *testing.T) {
	g := newGitRig(t)
	g.write(g.lap, "old.pem", "synthetic\n")
	g.run(g.lap, "add", "old.pem")
	g.run(g.lap, "commit", "-q", "-m", "pem")
	adds := g.run(g.lap, "rev-parse", "HEAD")
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		g.write(g.lap, name, "synthetic\n")
		g.run(g.lap, "add", name)
		g.run(g.lap, "commit", "-q", "-m", name)
	}
	tip := g.run(g.lap, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(g.lap, ".git", "objects", adds[:2], adds[2:])); err != nil {
		t.Fatalf("the commit is not a loose object: %v", err)
	}
	err := checkHistoryAllowed(t.Context(), g.lap, "feature/x", tip, []string{"old.pem"})
	var denied *HistoryDeniedError
	if err == nil || errors.As(err, &denied) || !strings.Contains(err.Error(), "rev-list") {
		t.Fatalf("got %v; want a rev-list error", err)
	}
}

// The scan reads nothing the laptop's environment names: a graft file set in the process hides a commit
// from a git that inherits it, not from the scan of the bundle.
func TestCheckBundleAllowed_IgnoresTheLaptopsEnvironment(t *testing.T) {
	g := newGitRig(t)
	g.write(g.lap, "old.pem", "synthetic\n")
	g.run(g.lap, "add", "old.pem")
	g.run(g.lap, "commit", "-q", "-m", "pem")
	g.run(g.lap, "rm", "-q", "old.pem")
	g.run(g.lap, "commit", "-q", "-m", "gone")
	tip, base := g.run(g.lap, "rev-parse", "HEAD"), g.run(g.lap, "rev-parse", "HEAD~2")
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	g.run(g.lap, "bundle", "create", "-q", bundle, "refs/heads/feature/x")
	grafts := filepath.Join(t.TempDir(), "grafts")
	g.write(filepath.Dir(grafts), "grafts", tip+" "+base+"\n")
	t.Setenv("GIT_GRAFT_FILE", grafts)
	err := checkBundleAllowed(t.Context(), bundle, "feature/x", tip, []string{"*.pem"})
	var denied *HistoryDeniedError
	if !errors.As(err, &denied) || denied.Path != "old.pem" {
		t.Fatalf("got %v; want a refusal naming old.pem", err)
	}
}

// gitProbe puts a git first on PATH that records each call's HOME, GIT_DEFAULT_HASH and arguments to the
// returned log, fails a bare init carrying --object-format=sha256 as a git older than 2.28 does, and passes
// everything else to the real git.
func gitProbe(t *testing.T) (log string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s|%s|%s\\n' \"$HOME\" \"$GIT_DEFAULT_HASH\" \"$*\" >> '" + log + "'\n" +
		"case \" $* \" in *' --object-format=sha256 '*) echo 'error: unknown option `object-format=sha256' >&2; exit 129;; esac\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// A git that cannot make a SHA-256 repository gives the clear refusal, and the bundle is not read.
func TestCheckBundleAllowed_RefusesWhenGitCannotMakeASHA256Scanner(t *testing.T) {
	log := gitProbe(t)
	err := checkBundleAllowed(t.Context(), filepath.Join(t.TempDir(), "missing.bundle"), "feature/x", strings.Repeat("a", 64), []string{"*.pem"})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 object names") || !strings.Contains(err.Error(), "cannot make a repository to check them in") {
		t.Fatalf("got %v; want the SHA-256 refusal", err)
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "fetch") {
		t.Fatalf("the bundle was read after the init failed:\n%s", b)
	}
}

// The scan's init names SHA-1 for a 40-hex tip, its fetch carries the maintenance and gc switches, and both run
// with HOME in the scan's own directory, not the laptop's; so do the listing's rev-list, diff-tree and ls-tree.
func TestCheckBundleAllowed_PinsTheScanInitAndFetch(t *testing.T) {
	g := newGitRig(t)
	log := gitProbe(t)
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	g.run(g.lap, "bundle", "create", "-q", bundle, "refs/heads/feature/x")
	if err := checkBundleAllowed(t.Context(), bundle, "feature/x", g.head, []string{"*.pem"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	var inits, fetches, listed int
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.SplitN(line, "|", 3)
		if len(f) != 3 {
			continue
		}
		switch {
		case strings.Contains(f[2], " init "):
			inits++
			if f[1] != "sha1" {
				t.Errorf("init ran with GIT_DEFAULT_HASH=%q, want sha1: %s", f[1], line)
			}
		case strings.Contains(f[2], " fetch "):
			fetches++
			for _, want := range []string{"-c maintenance.auto=false", "-c gc.auto=0"} {
				if !strings.Contains(f[2], want) {
					t.Errorf("fetch lacks %q: %s", want, line)
				}
			}
		case strings.Contains(f[2], " rev-list "), strings.Contains(f[2], " diff-tree "), strings.Contains(f[2], " ls-tree "):
			listed++
		default:
			continue
		}
		if !strings.Contains(f[0], "wardyn-bundle-scan-") {
			t.Errorf("HOME is %q, want the scan's own directory: %s", f[0], line)
		}
	}
	if inits != 1 || fetches != 1 {
		t.Fatalf("saw %d init and %d fetch calls, want 1 each:\n%s", inits, fetches, b)
	}
	if listed == 0 {
		t.Fatalf("saw no rev-list, diff-tree or ls-tree call:\n%s", b)
	}
}
