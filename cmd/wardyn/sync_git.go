// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
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

// syncGitSent is the branch and commit the last --git sent, kept in the sync state.
type syncGitSent struct {
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
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, syncShown(strings.TrimSpace(errb.String())))
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
	var out, errb cappedBuffer
	sess.Stdout, sess.Stderr = &out, &errb
	if err := sess.Run(script); err != nil {
		return "", fmt.Errorf("git in the sandbox: %w: %s", err, syncShown(strings.TrimSpace(errb.String())))
	}
	return strings.TrimSpace(out.String()), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func syncGitBundlePath(runID, side string) string {
	return path.Join(syncGitDir, runID+"-"+side+".bundle")
}

// syncGitCheck refuses a remote directory the bundles would land inside, and a
// laptop directory that is not the top of a work tree (the sandbox repository's
// files are the synced ones, so the two must start at the same place).
func syncGitCheck(ctx context.Context, sy *syncer, root string) error {
	if sy.o.remote == syncGitDir || strings.HasPrefix(sy.o.remote, syncGitDir+"/") {
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

// sendGit sends the laptop's current branch to the sandbox as a bundle, unless
// the last --git already sent that commit. A sandbox directory with no .git
// gets a fresh repository with the branch checked out over the synced files; one
// that has a repository gets the branch as refs/remotes/laptop/<branch>.
func sendGit(ctx context.Context, w io.Writer, sy *syncer, c *ssh.Client, runID, root string) error {
	branch, err := laptopGit(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return errors.New("--git needs a branch checked out on this computer (HEAD is detached)")
	}
	if !syncGitBranch.MatchString(branch) || strings.Contains(branch, "..") {
		return fmt.Errorf("--git does not send the branch name %s: use letters, digits, '.', '_', '-' and '/'", syncShown(branch))
	}
	head, err := laptopGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if sent := sy.st.GitSent; sent != nil && *sent == (syncGitSent{Branch: branch, Commit: head}) {
		fmt.Fprintf(w, "git: %s at %s is already in the sandbox\n", branch, head[:12])
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(sy.stateFile), "laptop-*.bundle")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if _, err := laptopGit(ctx, root, "bundle", "create", "-q", tmp.Name(), "refs/heads/"+branch); err != nil {
		return err
	}
	remote := syncGitBundlePath(runID, "laptop")
	if err := sy.uploadFile(tmp.Name(), remote); err != nil {
		return fmt.Errorf("send the git bundle: %w", err)
	}
	b := shellQuote(remote)
	out, err := sandboxGit(c, "set -eu; cd "+shellQuote(sy.o.remote)+"; "+
		"if [ -e .git ]; then git fetch -q --no-tags "+b+" "+shellQuote("+refs/heads/"+branch+":refs/remotes/laptop/"+branch)+"; echo existing; "+
		"else git init -q --template= && git fetch -q --no-tags "+b+" "+shellQuote("+refs/heads/"+branch+":refs/heads/"+branch)+
		" && git symbolic-ref HEAD "+shellQuote("refs/heads/"+branch)+" && git reset -q; echo fresh; fi; rm -f "+b)
	if err != nil {
		return err
	}
	sy.st.GitSent = &syncGitSent{Branch: branch, Commit: head}
	if err := sy.st.save(sy.stateFile); err != nil {
		return err
	}
	where := "refs/remotes/laptop/" + branch + " (the sandbox already had a repository)"
	if out == "fresh" {
		where = "a new repository, with " + branch + " checked out over the synced files"
	}
	fmt.Fprintf(w, "git: sent %s at %s to the sandbox as %s\n", branch, head[:12], where)
	return nil
}

// pullCommits brings the sandbox's commits past what --git last sent back as a
// bundle, checks it, and fetches it into wardyn/sync-<run> on this computer.
// The working tree and every other branch are left alone.
func pullCommits(ctx context.Context, w io.Writer, sy *syncer, c *ssh.Client, runID, root string) error {
	base := ""
	if sy.st.GitSent != nil && syncGitSHA.MatchString(sy.st.GitSent.Commit) {
		base = sy.st.GitSent.Commit
	}
	remote := syncGitBundlePath(runID, "sandbox")
	out, err := sandboxGit(c, "set -eu; cd "+shellQuote(sy.o.remote)+"; "+
		"[ -e .git ] || { echo 'no git repository in the synced directory; send one with --git' >&2; exit 3; }; "+
		"range=HEAD; if [ -n '"+base+"' ] && git cat-file -e '"+base+"^{commit}' 2>/dev/null; then range='HEAD ^"+base+"'; fi; "+
		"n=$(git rev-list --count $range); if [ \"$n\" = 0 ]; then echo 0; exit 0; fi; "+
		"mkdir -p "+shellQuote(syncGitDir)+"; git bundle create -q "+shellQuote(remote)+" $range; echo \"$n\"")
	if err != nil {
		return err
	}
	if out == "0" {
		fmt.Fprintln(w, "git: no new commits in the sandbox")
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(sy.stateFile), "sandbox-*.bundle")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	err = sy.downloadFile(remote, tmp.Name())
	_ = sy.c.Remove(remote)
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
	fmt.Fprintf(w, "git: fetched %s commit(s) from the sandbox into the branch %s; review it with 'git log %s'. Your working tree is unchanged\n", syncShown(out), branch, branch)
	return nil
}

// uploadFile writes the local file src to the absolute sandbox path dst.
func (sy *syncer) uploadFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := sy.c.MkdirAll(path.Dir(dst)); err != nil {
		return err
	}
	out, err := sy.c.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
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
func (sy *syncer) downloadFile(src, dst string) error {
	in, err := sy.c.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	limit := min(int64(maxPullFile), sy.o.maxPullBytes)
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
