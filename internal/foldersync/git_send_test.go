// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gitRig is a laptop repository with one commit on feature/x, its bundle, and
// a git wrapper that, at the git step named by its first argument, fails
// (FAIL_GIT), first runs the script HOOK_SH, once (HOOK_GIT), or kills the
// script after it (CUT_GIT). Reading HEAD (symbolic-ref -q) is not a step,
// and reading feature/x is the step read-branch.
type gitRig struct {
	t                           *testing.T
	git, bin, lap, bundle, head string
	sent                        []byte
	sha256                      bool
}

func newGitRig(t *testing.T) *gitRig { return newGitRigHash(t, false) }

// newGitRigHash is newGitRig with a laptop repository of the named hash. It skips when the host's git cannot
// make a SHA-256 repository (git older than 2.29).
func newGitRigHash(t *testing.T, sha256 bool) *gitRig {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	home := t.TempDir()
	// maintenance.auto=false: a git that runs its auto maintenance detached after a fetch or commit (newer releases
	// do; 2.43 runs it in the foreground) can still hold .git/objects/maintenance.lock when the test's temp dirs
	// are removed, and the cleanup then fails with "directory not empty".
	for k, v := range map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": filepath.Join(home, "none"),
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "maintenance.auto", "GIT_CONFIG_VALUE_0": "false",
		"GIT_AUTHOR_NAME": "Dev", "GIT_AUTHOR_EMAIL": "dev@example.com", "GIT_COMMITTER_NAME": "Dev", "GIT_COMMITTER_EMAIL": "dev@example.com"} {
		t.Setenv(k, v)
	}
	g := &gitRig{t: t, sha256: sha256, git: real, bin: t.TempDir(), lap: t.TempDir(), bundle: filepath.Join(t.TempDir(), "laptop.bundle")}
	wrapper := "#!/bin/sh\ns=$1; [ \"$1 ${2:-}\" = 'symbolic-ref -q' ] && s=read-HEAD\n[ \"$1 ${4:-}\" = 'rev-parse refs/heads/feature/x' ] && s=read-branch\n[ \"$s\" = \"${FAIL_GIT:-}\" ] && { echo \"git $1 failed\" >&2; exit 1; }\n" +
		"[ \"$s\" = \"${HOOK_GIT:-}\" ] && [ ! -e \"$HOOK_SH.ran\" ] && { : >\"$HOOK_SH.ran\"; HOOK_GIT= sh \"$HOOK_SH\"; }\n" + shellQuote(real) +
		" \"$@\"; rc=$?\n[ \"$s\" = \"${CUT_GIT:-}\" ] && kill -9 $PPID\nexit $rc\n"
	g.write(g.bin, "git", wrapper)
	if err := os.Chmod(filepath.Join(g.bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	initArgs := []string{"init", "-q", "-b", "feature/x"}
	if sha256 {
		initArgs = append(initArgs, "--object-format=sha256")
		if out, err := exec.Command(real, append([]string{"-C", g.lap}, initArgs...)...).CombinedOutput(); err != nil {
			t.Skipf("this git cannot make a SHA-256 repository: %v: %s", err, out)
		}
	} else {
		g.run(g.lap, initArgs...)
	}
	g.write(g.lap, "main.go", "package main\n")
	g.run(g.lap, "add", "main.go")
	g.run(g.lap, "commit", "-q", "-m", "first")
	g.head = g.run(g.lap, "rev-parse", "HEAD")
	g.run(g.lap, "bundle", "create", "-q", g.bundle, "refs/heads/feature/x")
	if g.sent, err = os.ReadFile(g.bundle); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *gitRig) run(dir string, args ...string) string {
	g.t.Helper()
	out, err := exec.Command(g.git, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g *gitRig) write(dir, name, body string) {
	g.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// box is a sandbox directory holding the synced files, below parent.
func (g *gitRig) box(parent string) string {
	dir := filepath.Join(parent, "work")
	g.write(dir, "main.go", "package main\n")
	return dir
}

// sh runs script with the wrapper first on PATH, plus env. A killed script's rc is -1.
func (g *gitRig) sh(script string, env ...string) (stdout, stderr string, rc int) {
	g.t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(append(os.Environ(), "PATH="+g.bin+string(os.PathListSeparator)+os.Getenv("PATH")), env...)
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	_ = cmd.Run()
	return strings.TrimSpace(o.String()), e.String(), cmd.ProcessState.ExitCode()
}

// send uploads the bundle again, as SendGit does, and runs the send.
func (g *gitRig) send(dir string, env ...string) (string, string, int) {
	g.write(filepath.Dir(g.bundle), filepath.Base(g.bundle), string(g.sent))
	return g.sh(sendGitScript(dir, g.bundle, "feature/x", g.sha256), env...)
}

func (g *gitRig) pull(dir string) (string, string, int) {
	return g.sh(pullGitScript(dir, "", filepath.Join(g.t.TempDir(), "s", "sandbox.bundle")))
}

// wantSetUp checks the send made dir a finished repository with feature/x
// checked out at the laptop's commit over the synced files.
func (g *gitRig) wantSetUp(dir, out, errs string, rc int) {
	g.t.Helper()
	if rc != 0 || out != "fresh" {
		g.t.Fatalf("send: out %q rc %d stderr %q; want fresh", out, rc, errs)
	}
	if h := g.run(dir, "symbolic-ref", "HEAD"); h != "refs/heads/feature/x" {
		g.t.Errorf("HEAD is %s, want refs/heads/feature/x", h)
	}
	if h := g.run(dir, "rev-parse", "HEAD"); h != g.head {
		g.t.Errorf("feature/x is at %s, want %s", h, g.head)
	}
	if st := g.run(dir, "status", "--porcelain"); st != "" {
		g.t.Errorf("the synced files differ from the checked-out branch:\n%s", st)
	}
	if exists(filepath.Join(dir, sendGitIncomplete)) {
		g.t.Error("the setup marker is still there")
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// cutPoints are where a send can be cut while it sets a repository up: the
// first two are made by hand (between the two mkdirs, and before git init),
// the rest kill the send right after that git step.
var cutPoints = []string{"between the two mkdirs", "before init", "init", "fetch", "symbolic-ref", "update-ref", "reset"}

// cut leaves dir as a send cut at point leaves it.
func (g *gitRig) cut(dir, point string) {
	g.t.Helper()
	switch point {
	case "between the two mkdirs":
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			g.t.Fatal(err)
		}
		return
	case "before init":
		if err := os.MkdirAll(filepath.Join(dir, sendGitIncomplete), 0o755); err != nil {
			g.t.Fatal(err)
		}
		return
	}
	if out, errs, rc := g.send(dir, "CUT_GIT="+point); rc != -1 || out != "" {
		g.t.Fatalf("a send cut after %s: out %q rc %d stderr %q; want it killed", point, out, rc, errs)
	}
	if !exists(filepath.Join(dir, sendGitIncomplete)) {
		g.t.Fatalf("a send cut after %s left no setup marker", point)
	}
}

// A send cut anywhere while it sets the repository up leaves a .git the next
// send finishes, never one it takes for a repository.
func TestSendGitScript_ACutSendIsFinishedByTheNext(t *testing.T) {
	for _, point := range cutPoints {
		t.Run(point, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			g.cut(dir, point)
			out, errs, rc := g.send(dir)
			g.wantSetUp(dir, out, errs, rc)
		})
	}
}

// A SHA-256 history makes a SHA-256 repository in the sandbox, and a SHA-1 one makes a SHA-1 repository even
// where the host's default hash is SHA-256.
func TestSendGitScript_SHA256FreshRepo(t *testing.T) {
	t.Run("a SHA-256 bundle", func(t *testing.T) {
		g := newGitRigHash(t, true)
		dir := g.box(t.TempDir())
		out, errs, rc := g.send(dir)
		g.wantSetUp(dir, out, errs, rc)
		if f := g.run(dir, "rev-parse", "--show-object-format"); f != "sha256" {
			t.Errorf("the sandbox repository uses %s, want sha256", f)
		}
	})
	t.Run("a SHA-1 bundle on a host whose default is SHA-256", func(t *testing.T) {
		g := newGitRig(t)
		t.Setenv("GIT_DEFAULT_HASH", "sha256")
		dir := g.box(t.TempDir())
		out, errs, rc := g.send(dir)
		g.wantSetUp(dir, out, errs, rc)
		if f := g.run(dir, "rev-parse", "--show-object-format"); f != "sha1" {
			t.Errorf("the sandbox repository uses %s, want sha1", f)
		}
	})
}

// A sandbox repository of the other hash refuses the send, finished or half made, and no ref is written.
func TestSendGitScript_HashMismatchWritesNoRef(t *testing.T) {
	for _, c := range []struct {
		name            string
		bundle256, mark bool
	}{
		{"a SHA-1 repository and a SHA-256 bundle", true, false},
		{"a half-made SHA-1 repository and a SHA-256 bundle", true, true},
		{"a SHA-256 repository and a SHA-1 bundle", false, false},
		{"a half-made SHA-256 repository and a SHA-1 bundle", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newGitRigHash(t, c.bundle256)
			dir := g.box(t.TempDir())
			initArgs := []string{"init", "-q"}
			if !c.bundle256 {
				initArgs = append(initArgs, "--object-format=sha256")
				if out, err := exec.Command(g.git, append([]string{"-C", dir}, initArgs...)...).CombinedOutput(); err != nil {
					t.Skipf("this git cannot make a SHA-256 repository: %v: %s", err, out)
				}
			} else {
				g.run(dir, initArgs...)
			}
			if c.mark {
				if err := os.MkdirAll(filepath.Join(dir, sendGitIncomplete), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cfg := func() string {
				b, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			wantCfg, wantFormat := cfg(), g.run(dir, "rev-parse", "--show-object-format")
			out, errs, rc := g.send(dir)
			if rc == 0 || out != "" {
				t.Fatalf("a send into a repository of the other hash: out %q rc %d stderr %q; want it refused", out, rc, errs)
			}
			if !(strings.Contains(errs, "sha1") && strings.Contains(errs, "sha256") && strings.Contains(errs, "remove .git")) {
				t.Errorf("stderr %q does not name both hashes and the way to start again", errs)
			}
			if strings.Contains(errs, "corrupt") {
				t.Errorf("stderr %q blames the pack, not the hash", errs)
			}
			// The repository is exactly as it was and git still opens it.
			if got := cfg(); got != wantCfg {
				t.Errorf("the refused send changed .git/config:\n%s\nwas:\n%s", got, wantCfg)
			}
			if got := g.run(dir, "rev-parse", "--show-object-format"); got != wantFormat {
				t.Errorf("the repository's object format is %s, was %s", got, wantFormat)
			}
			// Read the files, not through git: a half-made repository of the other hash may not open any more.
			for _, ref := range []string{"refs/remotes/laptop/feature/x", "refs/heads/feature/x", "packed-refs"} {
				if exists(filepath.Join(dir, ".git", ref)) {
					t.Errorf("the refused send wrote %s", ref)
				}
			}
		})
	}
}

// A half-made repository of the bundle's own hash is not initialised again, and the send finishes it.
func TestSendGitScript_SameHashHalfMadeRepoIsFinished(t *testing.T) {
	for _, sha256 := range []bool{false, true} {
		name := "sha1"
		if sha256 {
			name = "sha256"
		}
		t.Run(name, func(t *testing.T) {
			g := newGitRigHash(t, sha256)
			dir := g.box(t.TempDir())
			initArgs := []string{"init", "-q"}
			if sha256 {
				initArgs = append(initArgs, "--object-format=sha256")
			}
			g.run(dir, initArgs...)
			if err := os.MkdirAll(filepath.Join(dir, sendGitIncomplete), 0o755); err != nil {
				t.Fatal(err)
			}
			out, errs, rc := g.send(dir)
			g.wantSetUp(dir, out, errs, rc)
			if f := g.run(dir, "rev-parse", "--show-object-format"); f != name {
				t.Errorf("the repository uses %s, want %s", f, name)
			}
		})
	}
}

// oldGitPath is a PATH entry for a sandbox git older than 2.29: in a repository it prints rev-parse's
// --show-object-format back and succeeds, and it rejects --object-format=sha256 as unknown (rc 129).
func (g *gitRig) oldGitPath() string {
	old := g.t.TempDir()
	g.write(old, "git", "#!/bin/sh\n[ \"$1 ${2:-}\" = 'rev-parse --show-object-format' ] && [ -e .git/HEAD ] && { echo --show-object-format; exit 0; }\n"+
		"for a in \"$@\"; do [ \"$a\" = --object-format=sha256 ] && { echo \"error: unknown option 'object-format=sha256'\" >&2; exit 129; }; done\n"+
		"exec "+shellQuote(filepath.Join(g.bin, "git"))+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(old, "git"), 0o755); err != nil {
		g.t.Fatal(err)
	}
	return "PATH=" + old + string(os.PathListSeparator) + g.bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

// A git older than 2.29 prints an unknown rev-parse flag back and succeeds. It only knows SHA-1, so a half-made
// SHA-1 repository is finished as before.
func TestSendGitScript_OldGitHalfMadeSha1RepoIsFinished(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.run(dir, "init", "-q")
	if err := os.MkdirAll(filepath.Join(dir, sendGitIncomplete), 0o755); err != nil {
		t.Fatal(err)
	}
	out, errs, rc := g.send(dir, g.oldGitPath())
	g.wantSetUp(dir, out, errs, rc)
}

// A SHA-256 send to a sandbox whose git is older than 2.29 says so and gives no advice to remove .git: removing it
// cannot help, that git cannot make a SHA-256 repository.
func TestSendGitScript_OldGitCannotHoldSHA256(t *testing.T) {
	for _, mark := range []bool{false, true} {
		name := "a finished repository"
		if mark {
			name = "a half-made repository"
		}
		t.Run(name, func(t *testing.T) {
			g := newGitRigHash(t, true)
			dir := g.box(t.TempDir())
			g.run(dir, "init", "-q")
			if mark {
				if err := os.MkdirAll(filepath.Join(dir, sendGitIncomplete), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			out, errs, rc := g.send(dir, g.oldGitPath())
			if rc == 0 || out != "" {
				t.Fatalf("out %q rc %d stderr %q; want the send refused", out, rc, errs)
			}
			if !strings.Contains(errs, gitTooOld) || strings.Contains(errs, "remove .git") {
				t.Errorf("stderr %q; want %q and no advice to remove .git", errs, gitTooOld)
			}
		})
	}
}

// --pull-commits across hashes is refused before git verifies or fetches the bundle, naming both hashes, and no
// pack file is left in the laptop's repository.
func TestCheckPullHash_RefusesABundleOfTheOtherHash(t *testing.T) {
	for _, c := range []struct{ sandbox256, laptop256 bool }{{true, false}, {false, true}, {false, false}, {true, true}} {
		name := map[bool]string{false: "sha1", true: "sha256"}
		t.Run("a "+name[c.sandbox256]+" sandbox and a "+name[c.laptop256]+" laptop", func(t *testing.T) {
			g := newGitRigHash(t, c.sandbox256)
			laptop := t.TempDir()
			args := []string{"init", "-q"}
			if c.laptop256 {
				args = append(args, "--object-format=sha256")
				if out, err := exec.Command(g.git, append([]string{"-C", laptop}, args...)...).CombinedOutput(); err != nil {
					t.Skipf("this git cannot make a SHA-256 repository: %v: %s", err, out)
				}
			} else {
				g.run(laptop, args...)
			}
			err := checkPullHash(t.Context(), laptop, g.bundle)
			if c.sandbox256 == c.laptop256 {
				if err != nil {
					t.Fatalf("the same hash on both sides: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "sha1") || !strings.Contains(err.Error(), "sha256") {
				t.Fatalf("err %v; want a refusal naming both hashes", err)
			}
			if packs, _ := filepath.Glob(filepath.Join(laptop, ".git", "objects", "pack", "*")); len(packs) != 0 {
				t.Errorf("the refused pull left %v", packs)
			}
		})
	}
}

// A pull before the next send says the repository is not finished while its
// HEAD has no commit and removes nothing; once HEAD has one, it pulls as from
// any repository.
func TestPullGitScript_AnUnfinishedRepositoryIsPulledOnceItHasACommit(t *testing.T) {
	for _, point := range cutPoints {
		t.Run(point, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			g.cut(dir, point)
			out, errs, rc := g.pull(dir)
			switch point {
			case "between the two mkdirs":
				if rc != 3 || !strings.Contains(errs, "no git repository in the synced directory") {
					t.Errorf("pull of an empty .git: out %q rc %d stderr %q; want no repository, exit 3", out, rc, errs)
				}
			case "update-ref", "reset":
				if rc != 0 || out != "1" {
					t.Errorf("pull with the branch checked out: out %q rc %d stderr %q; want its 1 commit", out, rc, errs)
				}
			default:
				if rc != 3 || !strings.Contains(errs, gitPullNothing) {
					t.Errorf("pull: out %q rc %d stderr %q; want exit 3 and %q", out, rc, errs, gitPullNothing)
				}
				if !exists(filepath.Join(dir, sendGitIncomplete)) {
					t.Error("the pull removed the setup marker")
				}
			}
		})
	}
}

// A commit made in the sandbox in an unfinished repository is pulled before
// the next send, and that send keeps it checked out and puts the laptop's
// branch beside it.
func TestSendGitScript_ACommitInAnUnfinishedRepositoryIsKept(t *testing.T) {
	for _, point := range cutPoints[2:] {
		t.Run(point, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			g.cut(dir, point)
			g.write(dir, "agent.txt", "work\n")
			g.run(dir, "add", "agent.txt")
			g.run(dir, "commit", "-q", "-m", "sandbox work")
			made := g.run(dir, "rev-parse", "HEAD")
			if out, errs, rc := g.pull(dir); rc != 0 || out == "0" {
				t.Errorf("pull before the next send: out %q rc %d stderr %q; want the sandbox commit", out, rc, errs)
			}
			out, errs, rc := g.send(dir)
			if rc != 0 || out != "existing" {
				t.Fatalf("send: out %q rc %d stderr %q; want existing", out, rc, errs)
			}
			if h := g.run(dir, "rev-parse", "HEAD"); h != made {
				t.Errorf("HEAD moved from the sandbox commit %s to %s", made, h)
			}
			if l := g.run(dir, "rev-parse", "refs/remotes/laptop/feature/x"); l != g.head {
				t.Errorf("refs/remotes/laptop/feature/x = %s, want %s", l, g.head)
			}
			if exists(filepath.Join(dir, sendGitIncomplete)) {
				t.Error("the setup marker is still there")
			}
		})
	}
}

// A step that fails fails the send, keeps the .git with its marker and says
// so; the next send finishes it. A failed fetch into a repository already
// there leaves it with no marker.
func TestSendGitScript_AFailedStepKeepsTheRepositoryForTheNextSend(t *testing.T) {
	for _, step := range []string{"init", "bundle", "fetch", "cat-file", "symbolic-ref", "update-ref", "reset"} {
		t.Run(step, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			out, errs, rc := g.send(dir, "FAIL_GIT="+step)
			if rc == 0 || out != "" || !strings.Contains(errs, gitUnfinished) {
				t.Fatalf("a failed %s: out %q rc %d stderr %q; want a failed send saying %q", step, out, rc, errs, gitUnfinished)
			}
			if !exists(filepath.Join(dir, sendGitIncomplete)) {
				t.Fatalf("a failed %s did not keep the marked .git", step)
			}
			out, errs, rc = g.send(dir)
			g.wantSetUp(dir, out, errs, rc)
		})
	}
	t.Run("fetch into a repository already there", func(t *testing.T) {
		g := newGitRig(t)
		dir := g.box(t.TempDir())
		g.run(dir, "init", "-q")
		if out, errs, rc := g.send(dir, "FAIL_GIT=fetch"); rc == 0 || out != "" {
			t.Errorf("out %q rc %d stderr %q; want a failed send", out, rc, errs)
		}
		if !exists(filepath.Join(dir, ".git", "HEAD")) || exists(filepath.Join(dir, sendGitIncomplete)) {
			t.Error("the repository already there was changed")
		}
	})
}

// Two sends at once: one that read the branch before the other moved it
// fails, moves nothing back, and says so; the other's result stands. One
// that finds the other has finished as it ends has set up the same repository.
func TestSendGitScript_ASendThatLosesARaceMovesNothingBack(t *testing.T) {
	for _, c := range []struct {
		cut, at string
		fails   bool
	}{{"fetch", "symbolic-ref", true}, {"symbolic-ref", "update-ref", true}, {"symbolic-ref", "reset", false}} {
		t.Run("the other send runs at "+c.at, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			g.cut(dir, c.cut)
			g.write(g.lap, "two.go", "package main\n")
			g.run(g.lap, "add", "two.go")
			g.run(g.lap, "commit", "-q", "-m", "second")
			second := g.run(g.lap, "rev-parse", "HEAD")
			newer := filepath.Join(t.TempDir(), "newer.bundle")
			g.run(g.lap, "bundle", "create", "-q", newer, "refs/heads/feature/x")
			g.write(dir, "two.go", "package main\n")
			hooks := t.TempDir()
			g.write(hooks, "other.sh", sendGitScript(dir, g.bundle, "feature/x", false))
			g.write(hooks, "hook.sh", "cp "+shellQuote(newer)+" "+shellQuote(g.bundle)+" && sh "+shellQuote(filepath.Join(hooks, "other.sh"))+" >"+shellQuote(filepath.Join(hooks, "out"))+" 2>&1\n")
			out, errs, rc := g.send(dir, "HOOK_GIT="+c.at, "HOOK_SH="+filepath.Join(hooks, "hook.sh"))
			other, _ := os.ReadFile(filepath.Join(hooks, "out"))
			if strings.TrimSpace(string(other)) != "fresh" {
				t.Fatalf("the other send said %q; want fresh", other)
			}
			if c.fails && (rc == 0 || out != "" || !strings.Contains(errs, gitUnfinished)) {
				t.Errorf("the send that lost: out %q rc %d stderr %q; want a failed send saying %q", out, rc, errs, gitUnfinished)
			}
			if !c.fails && (rc != 0 || out != "fresh") {
				t.Errorf("the send the other finished beside: out %q rc %d stderr %q; want fresh", out, rc, errs)
			}
			if h := g.run(dir, "rev-parse", "refs/heads/feature/x"); h != second {
				t.Errorf("feature/x = %s, want the other send's %s", h, second)
			}
			if st := g.run(dir, "status", "--porcelain"); st != "" || exists(filepath.Join(dir, sendGitIncomplete)) {
				t.Errorf("the index is not the branch's, or the setup marker is still there:\n%s", st)
			}
		})
	}
}

// A send that moves the branch forward after another send finished, and then
// fails at the index, puts the marker back and says so: the next send gives
// the branch its index, where an unmarked .git would keep the older one.
func TestSendGitScript_ABranchMovedWithNoIndexKeepsTheMarker(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	hooks := t.TempDir()
	older := filepath.Join(hooks, "older.bundle")
	g.write(hooks, "older.bundle", string(g.sent))
	g.write(g.lap, "two.go", "package main\n")
	g.run(g.lap, "add", "two.go")
	g.run(g.lap, "commit", "-q", "-m", "second")
	g.head = g.run(g.lap, "rev-parse", "HEAD")
	g.run(g.lap, "bundle", "create", "-q", g.bundle, "refs/heads/feature/x")
	g.sent, _ = os.ReadFile(g.bundle)
	g.write(dir, "two.go", "package main\n")
	lock := filepath.Join(dir, ".git", "index.lock")
	g.write(hooks, "other.sh", sendGitScript(dir, older, "feature/x", false))
	g.write(hooks, "hook.sh", "sh "+shellQuote(filepath.Join(hooks, "other.sh"))+" >/dev/null 2>&1 && : >"+shellQuote(lock)+"\n")
	out, errs, rc := g.send(dir, "HOOK_GIT=symbolic-ref", "HOOK_SH="+filepath.Join(hooks, "hook.sh"))
	if rc == 0 || out != "" || !strings.Contains(errs, gitUnfinished) {
		t.Fatalf("a send whose index step failed: out %q rc %d stderr %q; want a failed send saying %q", out, rc, errs, gitUnfinished)
	}
	if !exists(filepath.Join(dir, sendGitIncomplete)) {
		t.Fatal("the branch moved, its index did not, and no setup marker is left for the next send")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	out, errs, rc = g.send(dir)
	g.wantSetUp(dir, out, errs, rc)
}

// A send cut before its index step, then a rewritten laptop branch: the next
// send keeps the commit the cut send checked out and gives it its index.
func TestSendGitScript_ARewrittenBranchAfterACutStillGetsAnIndex(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.cut(dir, "update-ref")
	g.run(g.lap, "commit", "-q", "--amend", "-m", "first, reworded")
	amended := g.run(g.lap, "rev-parse", "HEAD")
	g.run(g.lap, "bundle", "create", "-q", g.bundle, "refs/heads/feature/x")
	g.sent, _ = os.ReadFile(g.bundle)
	out, errs, rc := g.send(dir)
	if rc != 0 || out != "existing" {
		t.Fatalf("send: out %q rc %d stderr %q; want existing", out, rc, errs)
	}
	if h, l := g.run(dir, "rev-parse", "HEAD"), g.run(dir, "rev-parse", "refs/remotes/laptop/feature/x"); h != g.head || l != amended {
		t.Errorf("HEAD %s and refs/remotes/laptop/feature/x %s; want %s and %s", h, l, g.head, amended)
	}
	if st := g.run(dir, "status", "--porcelain"); st != "" || exists(filepath.Join(dir, sendGitIncomplete)) {
		t.Errorf("the repository was left with no index, or with its marker:\n%s", st)
	}
}

// A commit the sandbox makes while a send is at its index step stays on its
// branch and ends up checked out: the index step moves no ref. git's own
// post-index-change hook stands in for the sandbox committing at that moment.
func TestSendGitScript_ACommitMadeDuringTheIndexStepIsKept(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.cut(dir, "update-ref")
	g.write(dir, "agent.txt", "work\n")
	hooks := filepath.Join(dir, ".git", "hooks")
	g.write(hooks, "post-index-change", "#!/bin/sh\n[ -e .git/hooked ] && exit 0\n: >.git/hooked\n"+
		shellQuote(g.git)+" add agent.txt && "+shellQuote(g.git)+" commit -q -m 'sandbox work'\n")
	if err := os.Chmod(filepath.Join(hooks, "post-index-change"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, errs, rc := g.send(dir)
	if rc != 0 || out != "fresh" {
		t.Fatalf("send: out %q rc %d stderr %q; want fresh", out, rc, errs)
	}
	made := g.run(dir, "log", "--all", "-1", "--format=%H", "--grep=sandbox work")
	if made == "" || g.run(dir, "rev-parse", "HEAD") != made {
		t.Errorf("the sandbox commit %q is not on the checked-out branch: the send took it off", made)
	}
	if st := g.run(dir, "status", "--porcelain"); st != "" || exists(filepath.Join(dir, sendGitIncomplete)) {
		t.Errorf("the index is not the sandbox commit's, or the marker is left:\n%s", st)
	}
}

// A commit the sandbox makes on the branch HEAD is on, after the send has
// seen that branch empty and before it moves HEAD, keeps HEAD: the send
// fails and says so, and the next one takes the commit for the sandbox's work.
func TestSendGitScript_ACommitMadeAsHEADMovesKeepsHEAD(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.cut(dir, "fetch")
	g.write(dir, "agent.txt", "work\n")
	hook := filepath.Join(t.TempDir(), "commit.sh")
	g.write(filepath.Dir(hook), "commit.sh", shellQuote(g.git)+" add agent.txt && "+shellQuote(g.git)+" commit -q -m 'sandbox work'\n")
	out, errs, rc := g.send(dir, "HOOK_GIT=symbolic-ref", "HOOK_SH="+hook)
	made := g.run(dir, "log", "--all", "-1", "--format=%H", "--grep=sandbox work")
	if rc == 0 || out != "" || !strings.Contains(errs, gitUnfinished) || made == "" || g.run(dir, "rev-parse", "HEAD") != made {
		t.Fatalf("send: out %q rc %d stderr %q, sandbox commit %q; want a failed send saying %q and HEAD at that commit", out, rc, errs, made, gitUnfinished)
	}
	if out, errs, rc = g.send(dir); rc != 0 || out != "existing" || g.run(dir, "rev-parse", "HEAD") != made {
		t.Errorf("the next send: out %q rc %d stderr %q; want existing and HEAD still at the sandbox commit", out, rc, errs)
	}
	if out, errs, rc = g.pull(dir); rc != 0 || out != "1" {
		t.Errorf("pull: out %q rc %d stderr %q; want the sandbox commit", out, rc, errs)
	}
}

// A branch the sandbox committed on, with HEAD since moved to another branch,
// is the sandbox's too: the send writes neither, keeps what is staged, and
// puts the laptop's commit beside them. (Writing HEAD first, the send failed
// at the ancestry check with HEAD on the branch and the other branch's index.)
// With no sandbox commit on the branch the send carries on and moves HEAD.
func TestSendGitScript_ABranchWithSandboxCommitsKeepsHEADElsewhere(t *testing.T) {
	for _, on := range []string{"a branch at the laptop's commit", "an unborn branch, no index", "a branch, feature/x as the send left it"} {
		t.Run("HEAD on "+on, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			g.cut(dir, "reset")
			if on == "a branch, feature/x as the send left it" {
				g.run(dir, "checkout", "-q", "-b", "work")
				out, errs, rc := g.send(dir)
				g.wantSetUp(dir, out, errs, rc)
				return
			}
			g.write(dir, "agent.txt", "work\n")
			g.run(dir, "add", "agent.txt")
			g.run(dir, "commit", "-q", "-m", "sandbox work")
			made, want := g.run(dir, "rev-parse", "HEAD"), "refs/heads/work"
			if on == "an unborn branch, no index" {
				want = "refs/heads/new"
				g.run(dir, "symbolic-ref", "HEAD", want)
				if err := os.Remove(filepath.Join(dir, ".git", "index")); err != nil {
					t.Fatal(err)
				}
			} else {
				g.run(dir, "checkout", "-q", "-b", "work", "HEAD~1")
				g.write(dir, "staged.txt", "staged\n")
				g.run(dir, "add", "staged.txt")
			}
			out, errs, rc := g.send(dir)
			if rc != 0 || out != "existing" {
				t.Fatalf("send: out %q rc %d stderr %q; want existing", out, rc, errs)
			}
			if h, b, l := g.run(dir, "symbolic-ref", "HEAD"), g.run(dir, "rev-parse", "refs/heads/feature/x"), g.run(dir, "rev-parse", "refs/remotes/laptop/feature/x"); h != want || b != made || l != g.head {
				t.Errorf("HEAD %s, feature/x %s, refs/remotes/laptop/feature/x %s; want %s, %s, %s", h, b, l, want, made, g.head)
			}
			if want == "refs/heads/work" {
				if d := g.run(dir, "diff", "--cached", "--name-status", "HEAD"); d != "A\tstaged.txt" {
					t.Errorf("the index is not as the sandbox left it:\n%s", d)
				}
			}
			if exists(filepath.Join(dir, sendGitIncomplete)) {
				t.Error("the setup marker is still there")
			}
		})
	}
}

// A branch a newer send moved is not the sandbox's work. An older send that
// read HEAD on another branch before the newer one wrote HEAD and the branch,
// or that reads HEAD on the branch, fails and keeps the marker: the newer
// send, cut after its update-ref, built no index, and the next send does.
// (Taking the branch for the sandbox's, the older send said existing and
// removed the marker, and no later send built the index.)
func TestSendGitScript_ABranchANewerSendMovedIsNotTheSandboxsWork(t *testing.T) {
	for _, on := range []string{"the branch git init made", "another branch at the laptop's commit", "feature/x, unborn"} {
		t.Run("HEAD on "+on, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			switch on {
			case "another branch at the laptop's commit":
				g.cut(dir, "reset")
				g.run(dir, "checkout", "-q", "-b", "work")
			case "feature/x, unborn":
				g.cut(dir, "symbolic-ref")
			}
			hooks := t.TempDir()
			older := filepath.Join(hooks, "older.bundle")
			g.write(hooks, "older.bundle", string(g.sent))
			g.write(g.lap, "two.go", "package main\n")
			g.run(g.lap, "add", "two.go")
			g.run(g.lap, "commit", "-q", "-m", "second")
			g.head = g.run(g.lap, "rev-parse", "HEAD")
			g.run(g.lap, "bundle", "create", "-q", g.bundle, "refs/heads/feature/x")
			g.sent, _ = os.ReadFile(g.bundle)
			g.write(dir, "two.go", "package main\n")
			g.write(hooks, "newer.sh", sendGitScript(dir, g.bundle, "feature/x", false))
			g.write(hooks, "hook.sh", "CUT_GIT=update-ref sh "+shellQuote(filepath.Join(hooks, "newer.sh"))+" >/dev/null 2>&1 || :\n")
			out, errs, rc := g.sh(sendGitScript(dir, older, "feature/x", false), "HOOK_GIT=read-branch", "HOOK_SH="+filepath.Join(hooks, "hook.sh"))
			if rc == 0 || out != "" || !strings.Contains(errs, gitUnfinished) || !exists(filepath.Join(dir, sendGitIncomplete)) {
				t.Fatalf("the older send: out %q rc %d stderr %q, marker %v; want a failed send saying %q and the marker kept", out, rc, errs, exists(filepath.Join(dir, sendGitIncomplete)), gitUnfinished)
			}
			if h := g.run(dir, "rev-parse", "refs/heads/feature/x"); h != g.head {
				t.Fatalf("feature/x = %s, want the newer send's %s", h, g.head)
			}
			if exec.Command(g.git, "-C", dir, "rev-parse", "-q", "--verify", "refs/remotes/laptop/feature/x").Run() == nil {
				t.Fatal("the older send wrote refs/remotes/laptop/feature/x before it failed")
			}
			out, errs, rc = g.send(dir)
			g.wantSetUp(dir, out, errs, rc)
		})
	}
}

// A HEAD the sandbox moves to a third branch between that send's two reads
// stays there, and so does the sandbox's branch: the send writes neither and
// fails, and the next one takes them for the sandbox's.
// (Carrying on from the second read, it moved HEAD onto the sandbox's branch
// with the third branch's index, and failed.)
func TestSendGitScript_AHEADMovedBetweenTheReadsIsNotWritten(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.cut(dir, "reset")
	g.write(dir, "agent.txt", "work\n")
	g.run(dir, "add", "agent.txt")
	g.run(dir, "commit", "-q", "-m", "sandbox work")
	made := g.run(dir, "rev-parse", "HEAD")
	g.run(dir, "checkout", "-q", "-b", "work", "HEAD~1")
	hook := filepath.Join(t.TempDir(), "switch.sh")
	g.write(filepath.Dir(hook), "switch.sh", shellQuote(g.git)+" checkout -q -b other\n")
	for _, last := range []bool{false, true} {
		out, errs, _ := g.send(dir, "HOOK_GIT=read-branch", "HOOK_SH="+hook)
		if h, b := g.run(dir, "symbolic-ref", "HEAD"), g.run(dir, "rev-parse", "refs/heads/feature/x"); (last != (out == "existing")) || h != "refs/heads/other" || b != made {
			t.Fatalf("send: out %q stderr %q, HEAD %s, feature/x %s; want refs/heads/other, %s, a failed send, then existing", out, errs, h, b, made)
		}
	}
	if st := g.run(dir, "status", "--porcelain"); st != "" || exists(filepath.Join(dir, sendGitIncomplete)) {
		t.Errorf("the index is not HEAD's, or the setup marker is still there:\n%s", st)
	}
}

// A HEAD the sandbox detached is the sandbox's: the send keeps it, so a
// commit made on it while the send runs stays on HEAD, and the laptop's
// branch goes beside it.
func TestSendGitScript_ADetachedHEADIsKept(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.cut(dir, "reset")
	g.run(dir, "checkout", "-q", "--detach")
	g.write(dir, "agent.txt", "work\n")
	hook := filepath.Join(t.TempDir(), "commit.sh")
	g.write(filepath.Dir(hook), "commit.sh", shellQuote(g.git)+" add agent.txt && "+shellQuote(g.git)+" commit -q -m 'sandbox work'\n")
	out, errs, rc := g.send(dir, "HOOK_GIT=merge-base", "HOOK_SH="+hook)
	if rc != 0 || out != "existing" {
		t.Fatalf("send: out %q rc %d stderr %q; want existing", out, rc, errs)
	}
	made := g.run(dir, "log", "--all", "-1", "--format=%H", "--grep=sandbox work")
	if made == "" || g.run(dir, "rev-parse", "HEAD") != made {
		t.Errorf("the commit made on the detached HEAD %q is not HEAD: the send moved HEAD", made)
	}
	if l := g.run(dir, "rev-parse", "refs/remotes/laptop/feature/x"); l != g.head {
		t.Errorf("refs/remotes/laptop/feature/x = %s, want %s", l, g.head)
	}
	if out, errs, rc := g.sh(pullGitScript(dir, g.head, filepath.Join(t.TempDir(), "s.bundle"))); rc != 0 || out != "1" {
		t.Errorf("pull: out %q rc %d stderr %q; want the sandbox commit", out, rc, errs)
	}
}

// A send that finds HEAD already on the branch does not write it: another
// send that moved HEAD and then put it back on a branch that got a commit
// keeps it there, with that commit's index. The other send pauses after
// moving HEAD; a commit the sandbox made on the branch HEAD was on stands in
// for one that landed just before the move. This send pauses at its own
// HEAD write (which it no longer makes) or at its index step, until the
// other has put HEAD back; at the index step it then fails and says so.
func TestSendGitScript_HEADPutBackByAnotherSendStays(t *testing.T) {
	for _, at := range []string{"symbolic-ref", "reset"} {
		t.Run("this send pauses at "+at, func(t *testing.T) {
			g := newGitRig(t)
			dir := g.box(t.TempDir())
			g.cut(dir, "fetch")
			def := g.run(dir, "symbolic-ref", "HEAD")
			x := t.TempDir()
			f := func(n string) string { return shellQuote(filepath.Join(x, n)) }
			g.write(x, "this.bundle", string(g.sent))
			g.write(x, "this.sh", sendGitScript(dir, filepath.Join(x, "this.bundle"), "feature/x", false))
			g.write(x, "this-hook.sh", ": >"+f("paused")+"\ni=0; while [ ! -e "+f("other.done")+" ] && [ $i -lt 1000 ]; do sleep 0.01; i=$((i+1)); done\n")
			g.write(x, "other-hook.sh", "[ \"$(git symbolic-ref -q HEAD)\" = refs/heads/feature/x ] || { rm -f \"$0.ran\"; exit 0; }\n"+
				"y=$(printf 'work\\n' | git hash-object -w --stdin) && tr=$(printf '100644 blob %s\\tagent.txt\\n' \"$y\" | git mktree) && "+
				"c=$(git commit-tree -m 'sandbox work' \"$tr\") && git update-ref "+def+" \"$c\" '' || exit 1\n"+
				"(HOOK_GIT="+at+" HOOK_SH="+f("this-hook.sh")+" sh "+f("this.sh")+" >"+f("this.out")+" 2>"+f("this.err")+"; : >"+f("this.done")+") </dev/null >/dev/null 2>&1 &\n"+
				"i=0; while [ ! -e "+f("paused")+" ] && [ ! -e "+f("this.done")+" ] && [ $i -lt 1000 ]; do sleep 0.01; i=$((i+1)); done\n")
			_, errs, rc := g.send(dir, "HOOK_GIT=rev-parse", "HOOK_SH="+filepath.Join(x, "other-hook.sh"))
			g.write(x, "other.done", "")
			for i := 0; i < 1000 && !exists(filepath.Join(x, "this.done")); i++ {
				time.Sleep(10 * time.Millisecond)
			}
			thisOut, _ := os.ReadFile(filepath.Join(x, "this.out"))
			thisErr, _ := os.ReadFile(filepath.Join(x, "this.err"))
			if rc == 0 || !strings.Contains(errs, gitUnfinished) {
				t.Fatalf("the send that put HEAD back: rc %d stderr %q; want a failed send saying %q", rc, errs, gitUnfinished)
			}
			if at == "reset" && (strings.TrimSpace(string(thisOut)) != "" || !strings.Contains(string(thisErr), gitUnfinished)) {
				t.Errorf("this send, its HEAD put back while it built the index: out %q stderr %q; want a failed send saying %q", thisOut, thisErr, gitUnfinished)
			}
			made := g.run(dir, "rev-parse", def)
			if h := g.run(dir, "rev-parse", "HEAD"); h != made {
				t.Errorf("HEAD is %s, want the sandbox commit %s on %s; this send said %q %q", h, made, def, thisOut, thisErr)
			}
			if d := g.run(dir, "diff", "--cached", "--name-status", "HEAD"); d != "" {
				t.Errorf("the index is not HEAD's:\n%s", d)
			}
			if out, errs, rc := g.pull(dir); rc != 0 || out != "1" {
				t.Errorf("pull: out %q rc %d stderr %q; want the sandbox commit", out, rc, errs)
			}
		})
	}
}

// Residual: a HEAD the sandbox detaches and commits on in the moment between
// the send reading HEAD and writing it is not kept on HEAD. The commit stays
// in HEAD's reflog and on no branch, and the pull does not bring it back.
func TestSendGitScript_ACommitOnAHEADDetachedAsTheSendMovesItIsOnlyInTheReflog(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.cut(dir, "fetch")
	g.write(dir, "agent.txt", "work\n")
	hook := filepath.Join(t.TempDir(), "detach.sh")
	g.write(filepath.Dir(hook), "detach.sh", shellQuote(g.git)+" update-ref --no-deref HEAD "+g.head+" && "+
		shellQuote(g.git)+" add agent.txt && "+shellQuote(g.git)+" commit -q -m 'sandbox work'\n")
	out, errs, rc := g.send(dir, "HOOK_GIT=symbolic-ref", "HOOK_SH="+hook)
	if rc != 0 || out != "fresh" {
		t.Fatalf("send: out %q rc %d stderr %q; want fresh", out, rc, errs)
	}
	logged, made := g.run(dir, "reflog", "show", "--format=%H %gs", "HEAD"), ""
	for _, l := range strings.Split(logged, "\n") {
		if h, s, _ := strings.Cut(l, " "); s == "commit: sandbox work" {
			made = h
		}
	}
	if made == "" || g.run(dir, "for-each-ref", "--contains", made) != "" {
		t.Errorf("want the sandbox commit in HEAD's reflog and on no ref; reflog:\n%s", logged)
	}
	if out, errs, rc := g.sh(pullGitScript(dir, g.head, filepath.Join(t.TempDir(), "s.bundle"))); rc != 0 || out != "0" {
		t.Errorf("pull: out %q rc %d stderr %q; want 0", out, rc, errs)
	}
}

// No send removes what another writer left in an unfinished repository: a
// pack a running fetch is writing stays, and a lock a killed git left fails
// the send in git's own words and keeps everything; once the lock is gone,
// the next send finishes.
func TestSendGitScript_NothingAnotherWriterLeftIsRemoved(t *testing.T) {
	g := newGitRig(t)
	dir := g.box(t.TempDir())
	g.cut(dir, "fetch")
	pack := filepath.Join(dir, ".git", "objects", "pack", "tmp_pack_other")
	lock := filepath.Join(dir, ".git", "index.lock")
	g.write(filepath.Dir(pack), filepath.Base(pack), "half a pack")
	g.write(filepath.Dir(lock), filepath.Base(lock), "")
	out, errs, rc := g.send(dir)
	if rc == 0 || !strings.Contains(errs, "index.lock") || !strings.Contains(errs, gitUnfinished) {
		t.Fatalf("send with a lock left: out %q rc %d stderr %q; want git's lock message and %q", out, rc, errs, gitUnfinished)
	}
	if !exists(pack) || !exists(lock) || !exists(filepath.Join(dir, sendGitIncomplete)) {
		t.Fatal("the send removed what another writer left")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	out, errs, rc = g.send(dir)
	g.wantSetUp(dir, out, errs, rc)
	if !exists(pack) {
		t.Error("the send removed another writer's pack")
	}
}

// A .git the send did not make is kept, or refused with a message, and never
// removed; git never walks up into a parent repository. An empty .git is no
// repository, and the send sets one up in it.
func TestSendGitScript_ADotGitTheSendDidNotMakeIsKept(t *testing.T) {
	g := newGitRig(t)
	repo := func() string {
		d := t.TempDir()
		g.run(d, "init", "-q", "-b", "main")
		g.run(d, "commit", "-q", "--allow-empty", "-m", "own")
		return d
	}
	link := func(dir, target string) {
		if err := os.Symlink(target, filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, out, say, keep string
		rc, pullRC           int
		make                 func(dir string)
	}{
		{"a repository of the sandbox's own", "existing", "", "", 0, 0, func(dir string) {
			g.run(dir, "init", "-q", "-b", "main")
			g.run(dir, "commit", "-q", "--allow-empty", "-m", "own")
		}},
		{"a .git file", "existing", "", "", 0, 0, func(dir string) {
			if err := os.Remove(filepath.Join(dir, "main.go")); err != nil {
				t.Fatal(err)
			}
			g.run(repo(), "worktree", "add", "-q", dir)
		}},
		{"a link to a repository", "existing", "", "", 0, 0, func(dir string) { link(dir, filepath.Join(repo(), ".git")) }},
		{"a link to an unfinished repository", "existing", "", ".git/wardyn-setup-incomplete/keep", 0, 0, func(dir string) {
			r := repo()
			g.write(filepath.Join(r, sendGitIncomplete), "keep", "")
			link(dir, filepath.Join(r, ".git"))
		}},
		{"a link to nothing", "", gitDanglingLink, "", 1, 3, func(dir string) { link(dir, filepath.Join(t.TempDir(), "gone")) }},
		{"a .git that is not a repository", "", "not a git repository", ".git/notes", 128, 128, func(dir string) {
			g.write(filepath.Join(dir, ".git"), "notes", "mine\n")
		}},
		{"an empty .git", "fresh", "", "", 0, 0, func(dir string) { g.cut(dir, "between the two mkdirs") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			parent := t.TempDir()
			g.run(parent, "init", "-q")
			dir := g.box(parent)
			c.make(dir)
			before := snapshot(filepath.Join(dir, ".git"))
			out, errs, rc := g.send(dir)
			if out != c.out || rc != c.rc || !strings.Contains(errs, c.say) {
				t.Errorf("send: out %q rc %d stderr %q; want out %q rc %d saying %q", out, rc, errs, c.out, c.rc, c.say)
			}
			if after := snapshot(filepath.Join(dir, ".git")); c.out != "fresh" && after != before {
				t.Errorf(".git is now %s, was %s", after, before)
			}
			if c.keep != "" && !exists(filepath.Join(dir, c.keep)) {
				t.Errorf("%s was removed", c.keep)
			}
			if refs := g.run(parent, "for-each-ref"); refs != "" {
				t.Errorf("git wrote into the parent repository: %s", refs)
			}
			if _, errs, rc := g.pull(dir); rc != c.pullRC || (c.pullRC != 0 && !strings.Contains(errs, c.say)) {
				t.Errorf("pull: rc %d stderr %q; want rc %d", rc, errs, c.pullRC)
			}
		})
	}
}

// snapshot is what a send may not change in a .git it did not make: a link's
// target, a file's bytes, or a directory's HEAD.
func snapshot(p string) string {
	fi, err := os.Lstat(p)
	switch {
	case err != nil:
		return "missing"
	case fi.Mode()&os.ModeSymlink != 0:
		l, _ := os.Readlink(p)
		return "link to " + l
	case !fi.IsDir():
		b, _ := os.ReadFile(p)
		return "file " + strconv.Quote(string(b))
	}
	b, _ := os.ReadFile(filepath.Join(p, "HEAD"))
	return "directory, HEAD " + strconv.Quote(string(b))
}

// --git refuses a branch whose history holds a path the organisation's patterns
// leave out, even one a later commit removed or renamed away, and sends one
// that holds none.
func TestCheckHistoryAllowed_RefusesAPathTheOrganisationLeavesOut(t *testing.T) {
	g := newGitRig(t)
	ctx := t.Context()
	if err := checkHistoryAllowed(ctx, g.lap, "feature/x", g.head, []string{"*.pem", "secrets/"}); err != nil {
		t.Fatalf("a history with no matching path: %v; want it sent", err)
	}
	g.write(filepath.Join(g.lap, "secrets"), "notes.txt", "synthetic\n")
	g.write(g.lap, "old.pem", "synthetic\n")
	g.run(g.lap, "add", ".")
	g.run(g.lap, "commit", "-q", "-m", "add")
	g.run(g.lap, "mv", "old.pem", "kept.txt")
	g.run(g.lap, "rm", "-q", "-r", "secrets")
	g.run(g.lap, "commit", "-q", "-m", "remove and rename")
	tip := g.run(g.lap, "rev-parse", "HEAD")
	if st := g.run(g.lap, "ls-files"); st != "kept.txt\nmain.go" {
		t.Fatalf("the tip holds %q; the matching paths must be only in earlier commits", st)
	}
	for _, c := range []struct {
		deny              []string
		wantPath, wantPat string
	}{
		{[]string{"*.pem"}, "old.pem", "*.pem"},
		{[]string{"secrets/"}, "secrets/notes.txt", "secrets/"},
		{[]string{"*.pem", "!old.pem"}, "", ""},
		{[]string{"kept.txt"}, "kept.txt", "kept.txt"},
		{nil, "", ""},
	} {
		err := checkHistoryAllowed(ctx, g.lap, "feature/x", tip, c.deny)
		var denied *HistoryDeniedError
		switch {
		case c.wantPath == "" && err != nil:
			t.Errorf("deny %q: %v; want the history sent", c.deny, err)
		case c.wantPath != "" && (!errors.As(err, &denied) || denied.Path != c.wantPath || denied.Pattern != c.wantPat):
			t.Errorf("deny %q: got %v; want a refusal naming %s and %s", c.deny, err, c.wantPath, c.wantPat)
		}
	}
}

// moveBranchAround runs script (a commit that moves feature/x) when the git named by step runs, before (when
// after is false) or after it: a stand-in for a commit landing while --git is working.
func (g *gitRig) moveBranchAround(step string, after bool, script string) {
	g.t.Helper()
	hook := filepath.Join(g.t.TempDir(), "move.sh")
	g.write(filepath.Dir(hook), "move.sh", script)
	order := `sh "$MOVE_SH"; ` + shellQuote(g.git) + ` "$@"; rc=$?`
	if after {
		order = shellQuote(g.git) + ` "$@"; rc=$?; sh "$MOVE_SH"`
	}
	g.write(g.bin, "git", "#!/bin/sh\ncase \" $* \" in *\" "+step+" \"*) "+order+"; exit $rc;; esac\nexec "+shellQuote(g.git)+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(g.bin, "git"), 0o755); err != nil {
		g.t.Fatal(err)
	}
	g.t.Setenv("MOVE_SH", hook)
	g.t.Setenv("PATH", g.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func (g *gitRig) commitFileScript(name string) string {
	return "cd " + shellQuote(g.lap) + " && echo synthetic > " + name + " && " + shellQuote(g.git) + " add " + name + " && " + shellQuote(g.git) + " commit -q -m late\n"
}

// The commit --git sends is the one it checked. A commit landing on the branch after the head was read and
// before the bundle was made is refused, and one landing after the bundle was made is not in the file, which
// is checked at its own tip.
func TestMakeCheckedBundle_SendsExactlyTheCommitItChecked(t *testing.T) {
	deny := []string{"*.pem"}
	listHeads := func(g *gitRig, bundle string) string { return g.run(g.lap, "bundle", "list-heads", bundle) }
	t.Run("a commit lands before the bundle is made", func(t *testing.T) {
		g := newGitRig(t)
		bundle := filepath.Join(t.TempDir(), "b.bundle")
		g.moveBranchAround("bundle create", false, g.commitFileScript("late.pem"))
		err := makeCheckedBundle(t.Context(), g.lap, "feature/x", g.head, bundle, deny)
		if err == nil || !strings.Contains(err.Error(), "moved") {
			t.Fatalf("got %v; want a refusal saying the branch moved", err)
		}
	})
	t.Run("a commit lands after the bundle is made", func(t *testing.T) {
		g := newGitRig(t)
		bundle := filepath.Join(t.TempDir(), "b.bundle")
		g.moveBranchAround("bundle create", true, g.commitFileScript("late.pem"))
		if err := makeCheckedBundle(t.Context(), g.lap, "feature/x", g.head, bundle, deny); err != nil {
			t.Fatalf("the bundle holds only the checked commit, yet: %v", err)
		}
		if got, want := listHeads(g, bundle), g.head+" refs/heads/feature/x"; got != want {
			t.Fatalf("bundle heads = %q, want %q", got, want)
		}
		if now := g.run(g.lap, "rev-parse", "feature/x"); now == g.head {
			t.Fatal("the branch did not move; the test proves nothing")
		}
	})
	t.Run("a clean history is bundled at the checked tip", func(t *testing.T) {
		g := newGitRig(t)
		bundle := filepath.Join(t.TempDir(), "b.bundle")
		if err := makeCheckedBundle(t.Context(), g.lap, "feature/x", g.head, bundle, deny); err != nil {
			t.Fatal(err)
		}
		if got, want := listHeads(g, bundle), g.head+" refs/heads/feature/x"; got != want {
			t.Fatalf("bundle heads = %q, want %q", got, want)
		}
	})
	t.Run("a denied path in the checked commit's history", func(t *testing.T) {
		g := newGitRig(t)
		g.write(g.lap, "old.pem", "synthetic\n")
		g.run(g.lap, "add", "old.pem")
		g.run(g.lap, "commit", "-q", "-m", "pem")
		head := g.run(g.lap, "rev-parse", "HEAD")
		err := makeCheckedBundle(t.Context(), g.lap, "feature/x", head, filepath.Join(t.TempDir(), "b.bundle"), deny)
		var denied *HistoryDeniedError
		if !errors.As(err, &denied) || denied.Path != "old.pem" {
			t.Fatalf("got %v; want a refusal naming old.pem", err)
		}
	})
}

// historyRepo is a repository whose history holds, besides main.go: a root-commit file removed later, a file
// renamed away, and a file an evil merge adds that no parent has, removed by the next commit.
func (g *gitRig) historyRepo() (dir, tip string) {
	g.t.Helper()
	dir = g.t.TempDir()
	g.run(dir, "init", "-q", "-b", "feature/x")
	g.write(dir, "root.pem", "synthetic\n")
	g.write(dir, "base.txt", "synthetic\n")
	g.run(dir, "add", ".")
	g.run(dir, "commit", "-q", "-m", "root")
	g.run(dir, "branch", "side")
	g.run(dir, "rm", "-q", "root.pem")
	g.write(dir, "moved.pem", "synthetic\n")
	g.run(dir, "add", ".")
	g.run(dir, "commit", "-q", "-m", "second")
	g.run(dir, "mv", "moved.pem", "kept.txt")
	g.run(dir, "commit", "-q", "-m", "rename")
	g.run(dir, "checkout", "-q", "side")
	g.write(dir, "s.txt", "synthetic\n")
	g.run(dir, "add", ".")
	g.run(dir, "commit", "-q", "-m", "side")
	g.run(dir, "checkout", "-q", "feature/x")
	g.run(dir, "merge", "-q", "--no-ff", "--no-commit", "side")
	g.write(dir, "evil.pem", "synthetic\n")
	g.run(dir, "add", ".")
	g.run(dir, "commit", "-q", "-m", "merge")
	g.run(dir, "rm", "-q", "evil.pem")
	g.run(dir, "commit", "-q", "-m", "drop")
	return dir, g.run(dir, "rev-parse", "HEAD")
}

// The history scan names every path the old full listing did (the additions of each commit, a merge against
// each parent, are enough), and a user's git config cannot hide a merge's or the root's files from it.
func TestCheckHistoryAllowed_AdditionsNameEveryPathAndConfigCannotHideOne(t *testing.T) {
	g := newGitRig(t)
	dir, tip := g.historyRepo()
	names := func(args ...string) map[string]bool {
		out := map[string]bool{}
		for _, n := range strings.Split(g.run(dir, args...), "\x00") {
			if n = strings.TrimSpace(n); n != "" {
				out[n] = true
			}
		}
		return out
	}
	full := names("log", "-z", "--format=", "--name-only", "-m", "--no-renames", tip, "--")
	adds := names("log", "-z", "--format=", "--name-only", "-m", "--root", "--diff-filter=A", "--no-renames", tip, "--")
	for _, want := range []string{"root.pem", "moved.pem", "evil.pem", "base.txt", "s.txt", "kept.txt"} {
		if !full[want] || !adds[want] {
			t.Errorf("%s: in the full listing %v, in the additions %v; want both", want, full[want], adds[want])
		}
	}
	for n := range full {
		if !adds[n] {
			t.Errorf("%s is in the full listing but not in the additions", n)
		}
	}
	g.run(dir, "config", "log.diffMerges", "off")
	g.run(dir, "config", "log.showRoot", "false")
	for _, path := range []string{"evil.pem", "root.pem", "moved.pem"} {
		err := checkHistoryAllowed(t.Context(), dir, "feature/x", tip, []string{path})
		var denied *HistoryDeniedError
		if !errors.As(err, &denied) || denied.Path != path {
			t.Errorf("deny %s with log.diffMerges=off and log.showRoot=false: got %v; want a refusal naming it", path, err)
		}
	}
	if err := checkHistoryAllowed(t.Context(), dir, "feature/x", tip, []string{"*.nothing"}); err != nil {
		t.Errorf("a pattern nothing matches: %v; want the history sent", err)
	}
}

// SendGit itself refuses before anything is uploaded or recorded: a nil sftp client would fail the test if
// the refusal were skipped, and the state keeps its last sent commit.
func TestSendGit_RefusesADeniedHistoryBeforeUploadOrRecord(t *testing.T) {
	g := newGitRig(t)
	g.write(g.lap, "old.pem", "synthetic\n")
	g.run(g.lap, "add", "old.pem")
	g.run(g.lap, "commit", "-q", "-m", "pem")
	g.run(g.lap, "rm", "-q", "old.pem")
	g.run(g.lap, "commit", "-q", "-m", "drop")
	dir := t.TempDir()
	sy := &Syncer{State: &State{}, StateFile: filepath.Join(dir, "state.json"), StateRoot: dir, Opts: Options{OrgDeny: []string{"*.pem"}}}
	err := SendGit(t.Context(), io.Discard, sy, nil, "run", g.lap)
	var denied *HistoryDeniedError
	if !errors.As(err, &denied) || denied.Path != "old.pem" || denied.Pattern != "*.pem" {
		t.Fatalf("SendGit = %v; want a refusal naming old.pem and *.pem", err)
	}
	if sy.State.GitSent != nil {
		t.Errorf("GitSent = %+v after a refusal; want none", sy.State.GitSent)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "laptop-*.bundle")); len(left) != 0 {
		t.Errorf("a refused send left %v", left)
	}
}
