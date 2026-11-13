// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

// Git history for `wardyn sync --git` and `--pull-commits`. .git itself never
// crosses (syncDenied): its hooks and config run code. What crosses is a git
// bundle, which holds commits and branch names and nothing else. The laptop's
// branch is fetched into a repository the sandbox makes itself (git init with
// no template, so no hooks), and the sandbox's commits come back as a bundle
// the laptop fetches into a branch of its own, never into the working tree.

// syncGitDir is where the bundles wait in the sandbox, outside every synced directory.
const syncGitDir = "/home/agent/.wardyn-sync"

// syncGitBranch is the laptop branch names --git sends: they are spelled into
// a sandbox shell command, so only characters that mean nothing to it.
var syncGitBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

var syncGitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// GitSent is the branch and commit the last --git sent, kept in the sync state.
type GitSent struct {
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// laptopGit runs git in the laptop's synced directory.
func laptopGit(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = cliutil.ScrubChildEnv(os.Environ())
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, Shown(strings.TrimSpace(errb.String())))
	}
	return strings.TrimSpace(out.String()), nil
}

// sandboxGit runs script in the sandbox over a gateway exec channel and returns
// what it printed.
func sandboxGit(c *ssh.Client, script string) (string, error) {
	sess, err := c.NewSession()
	if err != nil {
		return "", fmt.Errorf("open an exec channel: %w", err)
	}
	defer sess.Close()
	var out, errb CappedBuffer
	sess.Stdout, sess.Stderr = &out, &errb
	if err := sess.Run(script); err != nil {
		return "", fmt.Errorf("git in the sandbox: %w: %s", err, Shown(strings.TrimSpace(errb.String())))
	}
	return strings.TrimSpace(out.String()), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func syncGitBundlePath(runID, side string) string {
	return path.Join(syncGitDir, runID+"-"+side+".bundle")
}

// GitCheck refuses a remote directory the bundles would land inside, and a
// laptop directory that is not the top of a work tree (the sandbox repository's
// files are the synced ones, so the two must start at the same place).
func GitCheck(ctx context.Context, sy *Syncer, root string) error {
	if sy.Opts.Remote == syncGitDir || strings.HasPrefix(sy.Opts.Remote, syncGitDir+"/") {
		return fmt.Errorf("--remote-dir may not be under %s, where --git keeps its bundles", syncGitDir)
	}
	top, err := laptopGit(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%s is not in a git work tree: %w", root, err)
	}
	a, aerr := filepath.EvalSymlinks(top)
	b, berr := filepath.EvalSymlinks(root)
	if aerr != nil || berr != nil || a != b {
		return fmt.Errorf("--git and --pull-commits need the top of the work tree (%s), not %s", top, root)
	}
	return nil
}

// SendGit sends the laptop's current branch to the sandbox as a bundle, unless
// the last --git already sent that commit. A sandbox directory with no .git
// gets a fresh repository with the branch checked out over the synced files; one
// that has a repository gets the branch as refs/remotes/laptop/<branch>.
func SendGit(ctx context.Context, w io.Writer, sy *Syncer, c *ssh.Client, runID, root string) error {
	branch, err := laptopGit(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return errors.New("--git needs a branch checked out on this computer (HEAD is detached)")
	}
	if !syncGitBranch.MatchString(branch) || strings.Contains(branch, "..") {
		return fmt.Errorf("--git does not send the branch name %s: use letters, digits, '.', '_', '-' and '/'", Shown(branch))
	}
	head, err := laptopGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if sent := sy.State.GitSent; sent != nil && *sent == (GitSent{Branch: branch, Commit: head}) {
		fmt.Fprintf(w, "git: %s at %s is already in the sandbox\n", branch, head[:12])
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(sy.StateFile), "laptop-*.bundle")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if _, err := laptopGit(ctx, root, "bundle", "create", "-q", tmp.Name(), "refs/heads/"+branch); err != nil {
		return err
	}
	remote := syncGitBundlePath(runID, "laptop")
	if err := sy.UploadFile(tmp.Name(), remote); err != nil {
		return fmt.Errorf("send the git bundle: %w", err)
	}
	out, err := sandboxGit(c, sendGitScript(sy.Opts.Remote, remote, branch))
	if err != nil {
		return err
	}
	sy.State.GitSent = &GitSent{Branch: branch, Commit: head}
	if err := sy.saveState(); err != nil {
		return err
	}
	where := "refs/remotes/laptop/" + branch + " (the sandbox already had a repository)"
	if out == "fresh" {
		where = "a new repository, with " + branch + " checked out over the synced files"
	}
	fmt.Fprintf(w, "git: sent %s at %s to the sandbox as %s\n", branch, head[:12], where)
	return nil
}

// sendGitIncomplete marks a .git a send is still setting up: it is made before
// git init writes anything and removed after the last step.
const sendGitIncomplete = ".git/wardyn-setup-incomplete"

// The sandbox's own words, on stderr, for a .git a send or pull leaves alone.
const (
	gitDanglingLink = ".git in the synced directory is a link to something that does not exist; it is left as it is: remove it or point it at a repository"
	gitUnfinished   = "this send did not finish setting up the repository in the synced directory; it is kept, and the next --git carries on with it if no other send has finished it"
	gitPullNothing  = "the repository in the synced directory is not finished and has no commits to bring back; send with --git to finish it"
)

// sendGitScript is the sandbox side of SendGit: it fetches bundle's branch into
// dir's repository, or sets one up. Setting up runs in an if condition, so a
// failed step fails the send (set -e ignores a failure inside an AND-list).
//
// No send removes a .git or anything in one but the marker: another send, or
// the sandbox's own git, may still be writing it. A send claims a .git it made
// or found empty with the marker, and every send that finds the marker
// repeats the steps, each safe to run again or beside another send: init
// again, the bundle's objects, HEAD to the branch, the branch, the index.
// The branch is written only by update-ref, forward and from the value just
// read; the index step (idx) is git reset with a path, which moves no ref
// (with no path, reset writes HEAD's branch back to the commit it read when
// it started, taking a commit the sandbox made meanwhile off its branch).
// A HEAD with commits the laptop's branch lacks, a detached one (only the
// sandbox detaches HEAD), or a <branch> with such commits while HEAD is on
// another branch, is the sandbox's: HEAD and every branch are kept, the
// laptop's branch goes to refs/remotes/laptop/<branch>, and the index is
// built only if there is none (and HEAD has a commit to build it from).
// A send that took <branch> for the sandbox's reads HEAD again first and
// fails if it moved: a newer send writes HEAD, then <branch>.
// HEAD is otherwise written only when it is on another branch; if that branch
// got a commit meanwhile, HEAD goes back to it with its index, and the send
// fails: the next one finds the commit as the sandbox's work. A send that
// moves the branch puts the marker back before the index step: another send
// may have finished and removed it. idx runs again, twice at most, when HEAD
// moved while it ran; a reset that fails fails the send, and so does a HEAD
// no longer on the branch once the index is built.
// GIT_DIR keeps git from walking up into a parent repository when .git is
// not one.
func sendGitScript(dir, bundle, branch string) string {
	b, m := shellQuote(bundle), shellQuote(sendGitIncomplete)
	ref, laptop := shellQuote("refs/heads/"+branch), shellQuote("refs/remotes/laptop/"+branch)
	return "set -eu; cd " + shellQuote(dir) + "; GIT_DIR=.git; export GIT_DIR; " +
		"idx() { c=$(git rev-parse -q --verify HEAD) && git reset -q \"$c\" -- . && " +
		"{ [ \"$(git rev-parse -q --verify HEAD)\" = \"$c\" ] || { [ \"${1:-0}\" -lt 2 ] && idx $((${1:-0}+1)); }; }; }; " +
		"if [ -L .git ] && [ ! -e .git ]; then echo " + shellQuote(gitDanglingLink) + " >&2; exit 1; fi; " +
		"mkdir .git 2>/dev/null || :; " +
		"if [ ! -L .git ] && { [ -d " + m + " ] || { [ -z \"$(ls -A .git 2>/dev/null)\" ] && mkdir " + m + " 2>/dev/null; } || [ -d " + m + " ]; }; then " +
		"if git init -q --template= && t=$(git bundle list-heads " + b + " " + ref + ") && t=${t%% *} && " +
		"git fetch -q --no-tags " + b + " " + ref + " && git cat-file -e \"$t^{commit}\" && " +
		"s= && if { h=$(git rev-parse -q --verify 'HEAD^{commit}') && { ! git merge-base --is-ancestor \"$h\" \"$t\" || ! git symbolic-ref -q HEAD >/dev/null; }; } || " +
		"{ p=$(git symbolic-ref -q HEAD) && [ \"$p\" != " + ref + " ] && o=$(git rev-parse -q --verify " + ref + ") && ! git merge-base --is-ancestor \"$o\" \"$t\" && s=$p; }; then " +
		"{ [ -z \"$s\" ] || [ \"$(git symbolic-ref -q HEAD)\" = \"$s\" ]; } && git update-ref " + laptop + " \"$t\" && { [ -e .git/index ] || [ -z \"$h\" ] || idx; } && out=existing; " +
		"else p=$(git symbolic-ref -q HEAD) && { [ \"$p\" = " + ref + " ] || { git symbolic-ref HEAD " + ref + " && " +
		"{ [ \"$(git rev-parse -q --verify \"$p\" || :)\" = \"$h\" ] || { git symbolic-ref HEAD \"$p\" && idx; false; }; }; }; } && " +
		"o=$(git rev-parse -q --verify " + ref + " || :) && " +
		"{ [ -z \"$o\" ] || git merge-base --is-ancestor \"$o\" \"$t\"; } && git update-ref " + ref + " \"$t\" \"$o\" && " +
		"{ [ \"$o\" = \"$t\" ] || mkdir " + m + " 2>/dev/null || :; } && idx && [ \"$(git symbolic-ref -q HEAD)\" = " + ref + " ] && out=fresh; fi && " +
		"{ rmdir " + m + " 2>/dev/null || [ ! -d " + m + " ]; }; then echo \"$out\"; " +
		"else echo " + shellQuote(gitUnfinished) + " >&2; exit 1; fi; " +
		"else git fetch -q --no-tags " + b + " " + shellQuote("+refs/heads/"+branch+":refs/remotes/laptop/"+branch) + "; echo existing; fi; rm -f " + b
}

// pullGitScript is the sandbox side of PullCommits: it bundles the commits on
// dir's HEAD past base into bundle and prints how many. An empty .git is no
// repository. One a send has not finished is pulled from once its HEAD has a
// commit; until then there is nothing to bring back, and it says so.
func pullGitScript(dir, base, bundle string) string {
	return "set -eu; cd " + shellQuote(dir) + "; GIT_DIR=.git; export GIT_DIR; " +
		"if [ -L .git ] && [ ! -e .git ]; then echo " + shellQuote(gitDanglingLink) + " >&2; exit 3; fi; " +
		"[ -n \"$(ls -A .git 2>/dev/null)\" ] || { echo 'no git repository in the synced directory; send one with --git' >&2; exit 3; }; " +
		"if [ ! -L .git ] && [ -d " + shellQuote(sendGitIncomplete) + " ] && ! git rev-parse -q --verify 'HEAD^{commit}' >/dev/null; then echo " + shellQuote(gitPullNothing) + " >&2; exit 3; fi; " +
		"range=HEAD; if [ -n '" + base + "' ] && git cat-file -e '" + base + "^{commit}' 2>/dev/null; then range='HEAD ^" + base + "'; fi; " +
		"n=$(git rev-list --count $range); if [ \"$n\" = 0 ]; then echo 0; exit 0; fi; " +
		"mkdir -p " + shellQuote(path.Dir(bundle)) + "; git bundle create -q " + shellQuote(bundle) + " $range; echo \"$n\""
}

// pullCommits brings the sandbox's commits past what --git last sent back as a
// bundle, checks it, and fetches it into wardyn/sync-<run> on this computer.
// The working tree and every other branch are left alone.
func PullCommits(ctx context.Context, w io.Writer, sy *Syncer, c *ssh.Client, runID, root string) error {
	base := ""
	if sy.State.GitSent != nil && syncGitSHA.MatchString(sy.State.GitSent.Commit) {
		base = sy.State.GitSent.Commit
	}
	remote := syncGitBundlePath(runID, "sandbox")
	out, err := sandboxGit(c, pullGitScript(sy.Opts.Remote, base, remote))
	if err != nil {
		return err
	}
	if out == "0" {
		fmt.Fprintln(w, "git: no new commits in the sandbox")
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(sy.StateFile), "sandbox-*.bundle")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	err = sy.DownloadFile(remote, tmp.Name())
	_ = sy.Client.Remove(remote)
	if err != nil {
		return fmt.Errorf("bring the git bundle back: %w", err)
	}
	if _, err := laptopGit(ctx, root, "bundle", "verify", "-q", tmp.Name()); err != nil {
		return err
	}
	branch := "wardyn/sync-" + runID[:8]
	if _, err := laptopGit(ctx, root, pullCommitsFetch(tmp.Name(), branch)...); err != nil {
		return err
	}
	fmt.Fprintf(w, "git: fetched %s commit(s) from the sandbox into the branch %s; review it with 'git log %s'. Your working tree is unchanged\n", Shown(out), branch, branch)
	return nil
}

// uploadFile writes the local file src to the absolute sandbox path dst.
func (sy *Syncer) UploadFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := sy.Client.MkdirAll(path.Dir(dst)); err != nil {
		return err
	}
	out, err := sy.Client.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// downloadFile reads the sandbox file src into the local file dst, under the
// same limits as a pulled file.
func (sy *Syncer) DownloadFile(src, dst string) error {
	in, err := sy.Client.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	limit := min(int64(MaxPullFile), sy.Opts.MaxPullBytes)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = fmt.Errorf("the bundle is over %d bytes (--max-pull-bytes, and 256 MiB for any one file)", limit)
	}
	return err
}

// pullCommitsFetch is the laptop fetch of a sandbox bundle into branch: every
// object checked, no tags, and no submodule the bundle names fetched.
func pullCommitsFetch(bundle, branch string) []string {
	return []string{"-c", "transfer.fsckObjects=true", "fetch", "-q", "--no-tags", "--no-recurse-submodules", bundle, "+HEAD:refs/heads/" + branch}
}

// stderrCap bounds the gateway's stderr kept for an error message; the rest is read and dropped.
const stderrCap = 64 << 10

// CappedBuffer keeps the first 64 KiB written to it and discards the rest, so the channel
// still drains and the gateway is never blocked on a full stderr.
type CappedBuffer struct{ buf bytes.Buffer }

func (c *CappedBuffer) Write(p []byte) (int, error) {
	if room := stderrCap - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *CappedBuffer) String() string { return c.buf.String() }
