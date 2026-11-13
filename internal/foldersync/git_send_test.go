// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
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
}

func newGitRig(t *testing.T) *gitRig {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	home := t.TempDir()
	for k, v := range map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": filepath.Join(home, "none"),
		"GIT_AUTHOR_NAME": "Dev", "GIT_AUTHOR_EMAIL": "dev@example.com", "GIT_COMMITTER_NAME": "Dev", "GIT_COMMITTER_EMAIL": "dev@example.com"} {
		t.Setenv(k, v)
	}
	g := &gitRig{t: t, git: real, bin: t.TempDir(), lap: t.TempDir(), bundle: filepath.Join(t.TempDir(), "laptop.bundle")}
	wrapper := "#!/bin/sh\ns=$1; [ \"$1 ${2:-}\" = 'symbolic-ref -q' ] && s=read-HEAD\n[ \"$1 ${4:-}\" = 'rev-parse refs/heads/feature/x' ] && s=read-branch\n[ \"$s\" = \"${FAIL_GIT:-}\" ] && { echo \"git $1 failed\" >&2; exit 1; }\n" +
		"[ \"$s\" = \"${HOOK_GIT:-}\" ] && [ ! -e \"$HOOK_SH.ran\" ] && { : >\"$HOOK_SH.ran\"; HOOK_GIT= sh \"$HOOK_SH\"; }\n" + shellQuote(real) +
		" \"$@\"; rc=$?\n[ \"$s\" = \"${CUT_GIT:-}\" ] && kill -9 $PPID\nexit $rc\n"
	g.write(g.bin, "git", wrapper)
	if err := os.Chmod(filepath.Join(g.bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	g.run(g.lap, "init", "-q", "-b", "feature/x")
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
	return g.sh(sendGitScript(dir, g.bundle, "feature/x"), env...)
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
			g.write(hooks, "other.sh", sendGitScript(dir, g.bundle, "feature/x"))
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
	g.write(hooks, "other.sh", sendGitScript(dir, older, "feature/x"))
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
			g.write(hooks, "newer.sh", sendGitScript(dir, g.bundle, "feature/x"))
			g.write(hooks, "hook.sh", "CUT_GIT=update-ref sh "+shellQuote(filepath.Join(hooks, "newer.sh"))+" >/dev/null 2>&1 || :\n")
			out, errs, rc := g.sh(sendGitScript(dir, older, "feature/x"), "HOOK_GIT=read-branch", "HOOK_SH="+filepath.Join(hooks, "hook.sh"))
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
			g.write(x, "this.sh", sendGitScript(dir, filepath.Join(x, "this.bundle"), "feature/x"))
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
