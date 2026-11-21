// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"bufio"
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
	return gitIn(ctx, root, laptopEnv(), args...)
}

// laptopEnv is the laptop's own environment, as its git sees it.
func laptopEnv() []string {
	return append(cliutil.ScrubChildEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0")
}

// cleanGitEnv is an environment in which git reads no configuration but the repository's own, no
// replace ref, no hook and nothing from a GIT_* variable the laptop set: home is an empty directory.
func cleanGitEnv(home string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0"}
	if v := os.Getenv("SYSTEMROOT"); v != "" {
		env = append(env, "SYSTEMROOT="+v)
	}
	return env
}

// gitVerb is the git command in args, past any leading -c name=value pairs.
func gitVerb(args []string) string {
	for len(args) > 2 && args[0] == "-c" {
		args = args[2:]
	}
	return args[0]
}

// gitIn runs git in dir with exactly env.
func gitIn(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", gitVerb(args), err, Shown(strings.TrimSpace(errb.String())))
	}
	return strings.TrimSpace(out.String()), nil
}

// laptopGitNames runs git in dir (with stdin as its input, if any) and hands visit each NUL-terminated name it
// prints, as it prints it, so a long history is never held whole. A visit error stops git and is returned.
func laptopGitNames(ctx context.Context, root string, env []string, stdin io.Reader, visit func(name string) error, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = env
	cmd.Stdin = stdin
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var verr error
	for verr == nil && sc.Scan() {
		if name := sc.Text(); name != "" {
			verr = visit(name)
		}
	}
	if verr != nil {
		cancel()
	}
	serr := sc.Err()
	werr := cmd.Wait()
	switch {
	case verr != nil:
		return verr
	case serr != nil:
		return fmt.Errorf("git %s: %w", gitVerb(args), serr)
	case werr != nil:
		return fmt.Errorf("git %s: %w: %s", gitVerb(args), werr, Shown(strings.TrimSpace(errb.String())))
	}
	return nil
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
	tmp, err := os.CreateTemp(filepath.Dir(sy.StateFile), "laptop-*.bundle")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	branch, head, sent, err := bundleCurrentBranch(ctx, root, tmp.Name(), sy.State.GitSent, sy.Opts.OrgDeny)
	if err != nil {
		return err
	}
	if sent {
		fmt.Fprintf(w, "git: %s at %s is already in the sandbox\n", branch, head[:12])
		return nil
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

// bundleCurrentBranch reads the branch and head to send and writes their checked bundle to path, unless
// already is that branch at that head (sent is then true and nothing is written). A branch that moves
// while its bundle is made is read and bundled once more; a second move is the caller's error.
func bundleCurrentBranch(ctx context.Context, root, path string, already *GitSent, orgDeny []string) (branch, head string, sent bool, err error) {
	for try := 0; ; try++ {
		branch, err = laptopGit(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return "", "", false, errors.New("--git needs a branch checked out on this computer (HEAD is detached)")
		}
		if !syncGitBranch.MatchString(branch) || strings.Contains(branch, "..") {
			return "", "", false, fmt.Errorf("--git does not send the branch name %s: use letters, digits, '.', '_', '-' and '/'", Shown(branch))
		}
		if head, err = laptopGit(ctx, root, "rev-parse", "HEAD"); err != nil {
			return "", "", false, err
		}
		if already != nil && *already == (GitSent{Branch: branch, Commit: head}) {
			return branch, head, true, nil
		}
		err = makeCheckedBundle(ctx, root, branch, head, path, orgDeny)
		var moved *branchMovedError
		if !errors.As(err, &moved) || try > 0 {
			return branch, head, false, err
		}
	}
}

// makeCheckedBundle writes branch's bundle to path and refuses it unless it carries exactly head and nothing
// the organisation's patterns leave out. The branch name can move at any moment, so the bundle is made
// first, its one advertised tip is read back from the file itself, and the history scanned is that
// commit's: what is uploaded is the file, whose contents no later commit can change.
func makeCheckedBundle(ctx context.Context, root, branch, head, path string, orgDeny []string) error {
	ref := "refs/heads/" + branch
	if _, err := laptopGit(ctx, root, "bundle", "create", "-q", path, ref); err != nil {
		return err
	}
	heads, err := laptopGit(ctx, root, "bundle", "list-heads", path)
	if err != nil {
		return err
	}
	tip, name, _ := strings.Cut(heads, " ")
	if name != ref || strings.ContainsAny(heads, "\n") || !syncGitSHA.MatchString(tip) {
		return fmt.Errorf("--git did not send %s: the bundle does not advertise exactly %s (%s)", Shown(branch), ref, Shown(heads))
	}
	if tip != head {
		return &branchMovedError{fmt.Sprintf("--git did not send %s: it moved from %s to %s while the bundle was made; run the sync again", Shown(branch), head[:12], tip[:12])}
	}
	if len(orgDeny) == 0 {
		return nil
	}
	if shallow, _ := laptopGit(ctx, root, "rev-parse", "--is-shallow-repository"); shallow == "true" {
		return fmt.Errorf("--git did not send %s: this repository is a shallow clone, so its history cannot be checked against your organisation's sync patterns; run git fetch --unshallow and send again", Shown(branch))
	}
	return checkBundleAllowed(ctx, path, branch, tip, orgDeny)
}

// checkBundleAllowed reads the bundle at path, the file that is sent, in an empty repository of its own,
// so nothing in the laptop's checkout (a replace ref, a graft, a config) changes what is inspected: the
// scan sees the commits the bundle's pack carries. The repository is made beside the bundle (a send's state
// directory, not a memory-backed temp one) and removed on every path.
func checkBundleAllowed(ctx context.Context, path, branch, tip string, orgDeny []string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(abs), "wardyn-bundle-scan-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	env := cleanGitEnv(dir)
	repo := filepath.Join(dir, "scan.git")
	// The scanner holds the bundle's own hash: a SHA-1 repository cannot import a SHA-256 bundle. SHA-1 takes
	// no flag, so an older git that lacks the option still works; the variable names SHA-1 for a git whose
	// default is another hash, and an older git ignores it.
	init, initEnv := []string{"init", "--bare", "-q", repo}, append(env[:len(env):len(env)], "GIT_DEFAULT_HASH=sha1")
	if len(tip) == 64 {
		init, initEnv = append(init, "--object-format=sha256"), env
	}
	if _, err := gitIn(ctx, dir, initEnv, init...); err != nil {
		if len(tip) == 64 {
			return fmt.Errorf("--git did not send %s: its history uses SHA-256 object names and this git cannot make a repository to check them in: %w", Shown(branch), err)
		}
		return err
	}
	ref := "refs/heads/" + branch
	if _, err := gitIn(ctx, repo, env, "-c", "core.hooksPath="+os.DevNull, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "fetch", "-q", "--no-tags", abs, ref+":"+ref); err != nil {
		return fmt.Errorf("--git did not send %s: its bundle could not be read back to check its history: %w", Shown(branch), err)
	}
	return checkHistoryAllowed(ctx, repo, branch, tip, orgDeny)
}

// branchMovedError is a branch that moved between the head SendGit read and the bundle's tip.
type branchMovedError struct{ msg string }

func (e *branchMovedError) Error() string { return e.msg }

// HistoryDeniedError is --git refusing a branch whose history holds a path the
// organisation's sync patterns leave out. A bundle carries every commit's files
// whole, so the patterns cannot be applied to it.
type HistoryDeniedError struct{ Branch, Path, Pattern string }

func (e *HistoryDeniedError) Error() string {
	return fmt.Sprintf("--git did not send %s: its history holds %s, which your organisation's sync pattern %s leaves out of a sync, and a git bundle carries every committed file. "+
		"The working-tree sync is not affected. To send the branch, remove that path from its history, or ask your operator to change the pattern",
		Shown(e.Branch), Shown(e.Path), Shown(e.Pattern))
}

// checkHistoryAllowed refuses to send branch, at the commit tip, when any path in its history, in
// any commit or in the tip's tree, is one the organisation's patterns deny,
// tested as a pass tests it: each directory above the file, then the file.
//
// Every path in any commit's tree was added by some commit against a parent (or sits in the root), so the
// additions of each commit, a merge against each parent, name them all, a removed or renamed-away one
// included (--no-renames keeps a rename as a delete and an add); the tip's tree is read as a second net.
// The listing is plumbing, rev-list into diff-tree, because git log prints what a user's config asks it
// to (a signature's text, a merge's format) among the names.
func checkHistoryAllowed(ctx context.Context, root, branch, tip string, orgDeny []string) error {
	if len(orgDeny) == 0 {
		return nil
	}
	ig := newSyncIgnore(orgDeny)
	seen := map[string]bool{}
	visit := func(name string) error {
		if seen[name] {
			return nil
		}
		seen[name] = true
		for i, c := range name {
			if c == '/' {
				if pat, denied := ig.orgDeniedBy(name[:i], true); denied {
					return &HistoryDeniedError{Branch: branch, Path: name, Pattern: pat}
				}
			}
		}
		if pat, denied := ig.orgDeniedBy(name, false); denied {
			return &HistoryDeniedError{Branch: branch, Path: name, Pattern: pat}
		}
		return nil
	}
	env := cleanGitEnv(root)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rev := exec.CommandContext(ctx, "git", "-C", root, "rev-list", tip, "--")
	rev.Env = env
	var revErr strings.Builder
	rev.Stderr = &revErr
	commits, err := rev.StdoutPipe()
	if err != nil {
		return err
	}
	if err := rev.Start(); err != nil {
		return err
	}
	if err := laptopGitNames(ctx, root, env, commits, visit, "diff-tree", "--stdin", "-z", "-r", "-m", "--root", "--no-renames",
		"--ignore-submodules=none", "--diff-filter=A", "--name-only", "--no-commit-id"); err != nil {
		cancel()
		_ = rev.Wait()
		return err
	}
	if err := rev.Wait(); err != nil {
		return fmt.Errorf("git rev-list: %w: %s", err, Shown(strings.TrimSpace(revErr.String())))
	}
	return laptopGitNames(ctx, root, env, nil, visit, "ls-tree", "-r", "-z", "--name-only", tip, "--")
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
	branch := reviewBranch(runID)
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

// reviewBranch is the laptop branch --pull-commits fetches a run's commits into. It holds the run's whole
// id, so two runs synced into one repository whose ids start alike never write to the same branch.
func reviewBranch(runID string) string { return "wardyn/sync-" + runID }

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
