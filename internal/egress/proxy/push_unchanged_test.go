// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// api answers the three GitHub REST reads the broker makes — a repository's
// default branch, a compare, one tree listing — from the bare repository
// itself, so the answer is what this forge holds, alternates included: that
// is how GitHub serves a fork network's objects through every repository in
// it.
func (f *gitForge) api(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.apiReads++
	f.apiAuth = r.Header.Get("Authorization")
	hook := f.apiHook
	f.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/", 3)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(f.root, parts[0], parts[1]+".git")
	var body any
	var ok bool
	switch rest := strings.Join(parts[2:], ""); {
	case rest == "":
		var head string
		head, ok = f.rev(dir, "symbolic-ref", "--short", "HEAD")
		body = map[string]string{"default_branch": head}
	case strings.HasPrefix(rest, "compare/"):
		body, ok = f.compare(dir, strings.TrimPrefix(rest, "compare/"))
	case strings.HasPrefix(rest, "git/trees/"):
		body, ok = f.tree(dir, strings.TrimPrefix(rest, "git/trees/"))
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// rev runs a git query in dir and returns its trimmed output.
func (f *gitForge) rev(dir string, args ...string) (string, bool) {
	out, err := f.git(dir, args...)
	return strings.TrimSpace(string(out)), err == nil
}

// compare answers "<branch>...<commit>" with the merge base, the one field of
// GitHub's compare the broker reads.
func (f *gitForge) compare(dir, spec string) (any, bool) {
	branch, head, _ := strings.Cut(spec, "...")
	b, ok1 := f.rev(dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	h, ok2 := f.rev(dir, "rev-parse", "--verify", "-q", head+"^{commit}")
	if !ok1 || !ok2 {
		return nil, false
	}
	mb, ok := f.rev(dir, "merge-base", b, h)
	if !ok {
		return nil, false
	}
	tree, ok := f.rev(dir, "rev-parse", mb+"^{tree}")
	return map[string]any{"merge_base_commit": map[string]any{
		"sha": mb, "commit": map[string]any{"tree": map[string]string{"sha": tree}}}}, ok
}

// tree lists one tree object as GitHub's git/trees does: one level, with
// ls-tree's modes, which are GitHub's (040000 for a directory).
func (f *gitForge) tree(dir, sha string) (any, bool) {
	if typ, ok := f.rev(dir, "cat-file", "-t", sha); !ok || typ != "tree" {
		return nil, false
	}
	out, err := f.git(dir, "ls-tree", "-z", sha)
	if err != nil {
		return nil, false
	}
	entries := []map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		meta, name, _ := strings.Cut(line, "\t")
		if fields := strings.Fields(meta); len(fields) == 3 {
			entries = append(entries, map[string]string{"path": name, "mode": fields[0], "type": fields[1], "sha": fields[2]})
		}
	}
	return map[string]any{"sha": sha, "tree": entries, "truncated": false}, true
}

func (f *gitForge) reads() (n int, auth string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiReads, f.apiAuth
}

func (f *gitForge) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
}

// landed reports whether the forge's run branch is exactly the local HEAD, so
// an allowed push is proven to have been forwarded, not merely not refused.
func (r *forgeRun) landed(t *testing.T) bool {
	t.Helper()
	forge, _ := exec.Command("git", "-C", r.bare, "rev-parse", "-q", "--verify", r.ref).Output()
	local, err := exec.Command("git", "-C", r.work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(forge)) == strings.TrimSpace(string(local))
}

// wantAllowed asserts a push went through and landed on the forge.
func (r *forgeRun) wantAllowed(t *testing.T, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("git push was refused, want it allowed: %v\n%s\n%s\nrefusal: %s",
			err, out, r.decisions, r.lastRefusal())
	}
	if !r.landed(t) {
		t.Errorf("git push succeeded but the forge's run branch is not the pushed commit\n%s", out)
	}
}

// wantRefusedFor is wantRefused plus the reason the refusal must give.
func (r *forgeRun) wantRefusedFor(t *testing.T, out string, err error, path, why string) {
	t.Helper()
	r.wantRefused(t, out, err, path)
	if r.landed(t) {
		t.Errorf("the refused commit is on the forge's run branch")
	}
	if body := r.lastRefusal(); !strings.Contains(body, why) {
		t.Errorf("refusal %q does not say %q", body, why)
	}
}

// ordinaryRepo is the repository the motivating case is about: it already has
// a workflow, a certificate, a root-level file and the source a run edits.
var ordinaryRepo = map[string]string{
	".github/workflows/ci.yml": "on: push\n",
	"certs/server.pem":         "-----BEGIN CERTIFICATE-----\n",
	"Makefile":                 "all:\n\techo hi\n",
	"src/main.go":              bigFile(200),
}

// editMain commits a one-line edit to src/main.go.
func (r *forgeRun) editMain(t *testing.T, marker string) {
	t.Helper()
	writeFiles(t, r.work, map[string]string{
		"src/main.go": strings.Replace(bigFile(200), "line 00100:", "line 00100! "+marker, 1),
	})
	r.commit(t, "edit src/main.go")
}

// asHuman commits files onto branch in the bare repository outside the run —
// a person pushing to the forge directly — starting from start, and returns
// the new commit.
func (r *forgeRun) asHuman(t *testing.T, start, branch string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "human")
	runGit(t, t.TempDir(), "clone", "-q", r.bare, dir)
	runGit(t, dir, "checkout", "-q", "-B", branch, start)
	writeFiles(t, dir, files)
	commitAll(t, dir)
	runGit(t, dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	return gitOut(t, dir, "rev-parse", "HEAD")
}

// commitAll stages and commits everything in dir.
func commitAll(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "by a person")
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// TestPushRulesPassWhatAPushLeavesUnchanged is the case push_rules exists for:
// a repository that already has the paths a rule protects, and a run that
// edits something else. The push leaves every denied path exactly as the
// commit it builds on holds it, so it must pass — on the first push, which
// creates the run's branch, and on the next, which updates it.
func TestPushRulesPassWhatAPushLeavesUnchanged(t *testing.T) {
	for _, deny := range []string{".github/workflows/**", "**/*.pem", "Makefile"} {
		t.Run(deny+" with an edit to src/main.go", func(t *testing.T) {
			r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, deny)
			r.editMain(t, "first")
			out, err := r.push()
			r.wantAllowed(t, out, err)

			r.editMain(t, "second")
			out, err = r.push()
			r.wantAllowed(t, out, err)
		})
	}
	t.Run("on the token lane to github.com", func(t *testing.T) {
		r := newForgeRunVia(t, githubHost, ordinaryRepo, []string{"--depth", "1"}, ".github/workflows/**")
		r.editMain(t, "first")
		out, err := r.push()
		r.wantAllowed(t, out, err)
	})
}

// TestPushRulesReadTheForgeWithTheRunsOwnCredential pins when the forge is
// read and with what: only for a verdict that depends on what the pack does not
// carry, with the lane's credential — the one the clone and the push's
// discovery already minted, so nothing new is minted for it.
func TestPushRulesReadTheForgeWithTheRunsOwnCredential(t *testing.T) {
	r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, ".github/workflows/**")
	if n := r.forge.mintCount(); n != 1 {
		t.Fatalf("mints after the clone = %d, want 1", n)
	}

	writeFiles(t, r.work, map[string]string{".github/workflows/ci.yml": "on: [push, pull_request]\n"})
	r.commit(t, "edit the workflow")
	out, err := r.push()
	r.wantRefused(t, out, err, ".github/workflows/ci.yml")
	if n, _ := r.forge.reads(); n != 0 {
		t.Errorf("a push refused from its own bytes read the forge %d time(s)", n)
	}
	forgeRow := `"rule_source":"` + ruleSourceGitForgeRead + `"`
	if strings.Contains(r.decisions.String(), forgeRow) {
		t.Errorf("a push that read nothing from the forge recorded a %s row", ruleSourceGitForgeRead)
	}

	runGit(t, r.work, "reset", "-q", "--hard", "HEAD~1")
	r.editMain(t, "allowed")
	out, err = r.push()
	r.wantAllowed(t, out, err)
	n, auth := r.forge.reads()
	if n == 0 || auth != "Bearer gh-tok" {
		t.Errorf("forge reads = %d with Authorization %q, want some with the lane's own token", n, auth)
	}
	// Issue #508 F6: the broker's own credentialed reads are in the run's
	// decision stream, not only on the sidecar's log line.
	var rows []egress.DecisionLog
	for _, line := range strings.Split(strings.TrimSpace(r.decisions.String()), "\n") {
		var d egress.DecisionLog
		if json.Unmarshal([]byte(line), &d) == nil && d.RuleSource == ruleSourceGitForgeRead {
			rows = append(rows, d)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want exactly one for the push that read the forge\n%s", ruleSourceGitForgeRead, len(rows), r.decisions)
	}
	if d := rows[0]; d.Decision != egress.Allow || d.Request.Host != githubAPIHost || d.Request.Port != 443 || d.Request.RunID == uuid.Nil {
		t.Errorf("forge-read row = %+v, want an ALLOW for this run naming %s:443", d, githubAPIHost)
	}
	if n := r.forge.mintCount(); n != 1 {
		t.Errorf("mints after reading the forge = %d, want still 1", n)
	}
}

// TestPushRulesCompareWithWhatTheRepositoryVouchesFor pins WHICH commit an
// entry the pack does not carry is compared with: one the push builds on, and
// only if the forge reports it inside the history of the default branch or of
// a branch the push updates. The forge holding the same tree at the same path
// somewhere else — on another branch, or in a fork — is not enough.
func TestPushRulesCompareWithWhatTheRepositoryVouchesFor(t *testing.T) {
	const deny = ".github/workflows/**"
	// A workflow the repository's default branch has never held.
	added := map[string]string{".github/workflows/deploy.yml": "on: push\njobs: {deploy: {runs-on: ubuntu-latest}}\n"}

	// A clone whose base is no longer a tip the forge advertises re-sends its
	// history, down to its shallow boundary or its root. The forge already
	// holds that history, so it is not what the push changes.
	t.Run("the default branch moved on after the clone", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, deny)
		r.asHuman(t, "main", "main", map[string]string{".github/workflows/ci.yml": "on: [push]\n"})
		r.editMain(t, "late")
		out, err := r.push()
		r.wantAllowed(t, out, err)
	})
	t.Run("the default branch moved on under a full clone with history of its own", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, nil, deny)
		r.asHuman(t, "main", "main", map[string]string{"README.md": "hello\n"})
		runGit(t, r.work, "pull", "-q", "--ff-only")
		r.editMain(t, "one")
		r.editMain(t, "two")
		r.asHuman(t, "main", "main", map[string]string{".github/workflows/ci.yml": "on: [push]\n"})
		out, err := r.push()
		r.wantAllowed(t, out, err)
	})
	t.Run("the default branch moved on and the push adds a workflow", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, deny)
		r.asHuman(t, "main", "main", map[string]string{"README.md": "hello\n"})
		writeFiles(t, r.work, added)
		r.commit(t, "add a workflow")
		r.editMain(t, "cover")
		out, err := r.push()
		r.wantRefused(t, out, err, ".github/workflows/deploy.yml")
	})
	t.Run("merging the default branch in after it changed a denied path", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, deny)
		r.editMain(t, "first")
		if out, err := r.push(); err != nil {
			t.Fatalf("first push: %v\n%s", err, out)
		}
		r.asHuman(t, "main", "main", map[string]string{".github/workflows/ci.yml": "on: [push]\n"})
		runGit(t, r.work, "fetch", "-q", "origin", "main")
		runGit(t, r.work, "merge", "-q", "--no-edit", "FETCH_HEAD")
		out, err := r.push()
		r.wantAllowed(t, out, err)
	})
	// The forge holds this exact tree at this exact path, on a branch outside
	// the default branch's history, and the commit the push builds on does not
	// hold it. git sends the tree again here, so the push is refused from its
	// own bytes; whichever way it is refused, it must be.
	t.Run("a tree another branch holds at the same path, on a commit that does not", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, deny)
		r.asHuman(t, "main", "other", added)
		runGit(t, r.work, "fetch", "-q", "origin", "other")
		runGit(t, r.work, "checkout", "-q", "FETCH_HEAD", "--", ".github")
		r.editMain(t, "carry")
		out, err := r.push()
		r.wantRefused(t, out, err, ".github/workflows/deploy.yml")
	})
	// A merge passes an entry that matches ANY parent the repository vouches
	// for, and only those: a parent outside the default branch's history does
	// not count.
	t.Run("a merge of a branch outside the default branch's history", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, deny)
		r.asHuman(t, "main", "other", added)
		runGit(t, r.work, "fetch", "-q", "origin", "other")
		runGit(t, r.work, "merge", "-q", "--no-ff", "--no-commit", "FETCH_HEAD")
		r.editMain(t, "merge")
		out, err := r.push()
		r.wantRefusedFor(t, out, err, ".github/workflows/deploy.yml", whyChanged)
	})
	// Stated, not accidental: a push may build on any commit in the default
	// branch's own history, and keeps what that commit held at a denied path.
	// The rule is that a push does not CHANGE a denied path relative to the
	// commit it builds on, not that every branch carries the newest version.
	t.Run("building on an older commit of the default branch keeps its workflow", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, nil, deny)
		old := gitOut(t, r.work, "rev-parse", "HEAD")
		r.asHuman(t, "main", "main", map[string]string{".github/workflows/ci.yml": "on: [push]\n"})
		runGit(t, r.work, "fetch", "-q", "origin", "main")
		runGit(t, r.work, "checkout", "-q", old)
		r.editMain(t, "old base")
		out, err := r.push()
		r.wantAllowed(t, out, err)
	})
	t.Run("a commit only a fork holds, named as the parent", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, deny)
		fork := r.forkWith(t, added)
		// A commit built by hand: the fork's .github tree in its root and the
		// fork's commit as its parent, neither of them in the run's clone.
		ghTree := gitOut(t, fork, "rev-parse", "HEAD:.github")
		var root strings.Builder
		for _, line := range strings.Split(gitOut(t, r.work, "ls-tree", "HEAD"), "\n") {
			if strings.HasSuffix(line, "\t.github") {
				line = "040000 tree " + ghTree + "\t.github"
			}
			root.WriteString(line + "\n")
		}
		tree := gitIn(t, r.work, root.String(), "mktree", "--missing")
		commit := gitIn(t, r.work, fmt.Sprintf("tree %s\nparent %s\nauthor T <t@e> 1767225600 +0000\n"+
			"committer T <t@e> 1767225600 +0000\n\nrestore\n", tree, gitOut(t, fork, "rev-parse", "HEAD")),
			"hash-object", "-t", "commit", "-w", "--stdin")
		status, body := r.postPack(t, commit, commit, tree)
		if status != http.StatusForbidden || !strings.Contains(body, whyNoBase) {
			t.Errorf("status %d, body %q: want 403 saying %q", status, body, whyNoBase)
		}
		r.wantRefused(t, body, fmt.Errorf("status %d", status), ".github/workflows/deploy.yml")
	})
}

// forkWith makes a fork of the forge's repository whose history carries
// files, and shares its objects with the repository the way GitHub's fork
// networks do: the repository reads them through alternates, and so do its
// REST API and its receive-pack. Returns a work tree of the fork.
func (r *forgeRun) forkWith(t *testing.T, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(r.forge.root, "someone", "hello-world.git")
	runGit(t, t.TempDir(), "clone", "-q", "--bare", r.bare, bare)
	work := filepath.Join(t.TempDir(), "fork")
	runGit(t, t.TempDir(), "clone", "-q", bare, work)
	writeFiles(t, work, files)
	commitAll(t, work)
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	alt := filepath.Join(r.bare, "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(bare, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return work
}

// gitIn runs git in dir with stdin and returns its trimmed output.
func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, gitEnv(), strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// postPack sends a hand-built push — exactly objects, creating the run's
// branch at tip — to the broker, and returns what it answered.
func (r *forgeRun) postPack(t *testing.T, tip string, objects ...string) (int, string) {
	t.Helper()
	pk := exec.Command("git", "pack-objects", "--stdout")
	pk.Dir, pk.Env, pk.Stdin = r.work, gitEnv(), strings.NewReader(strings.Join(objects, "\n")+"\n")
	pack, err := pk.Output()
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("0", 40) + " " + tip + " " + r.ref + "\x00report-status object-format=sha1\n"
	resp, err := http.Post(r.brokerURL+"/wardyn/gh/octocat/hello-world/git-receive-pack",
		"application/x-git-receive-pack-request",
		strings.NewReader(fmt.Sprintf("%04x%s0000", len(line)+4, line)+string(pack)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestPushRulesKeepTheStrictReadingWhenTheForgeCannotBeRead: every way the
// comparison can fail to finish refuses the push, the way it was refused
// before the forge was asked, and the refusal names the reason.
func TestPushRulesKeepTheStrictReadingWhenTheForgeCannotBeRead(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *gitForge)
		why   string
	}{
		{"the forge answers 500", func(t *testing.T, f *gitForge) {
			f.apiHook = func(w http.ResponseWriter, _ *http.Request) bool {
				http.Error(w, "unavailable", http.StatusInternalServerError)
				return true
			}
		}, "answered 500"},
		{"a tree listing is truncated", func(t *testing.T, f *gitForge) {
			f.apiHook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.Contains(r.URL.Path, "/git/trees/") {
					return false
				}
				_, _ = fmt.Fprint(w, `{"tree":[],"truncated":true}`)
				return true
			}
		}, "truncated"},
		{"the forge is slower than the broker waits", func(t *testing.T, f *gitForge) {
			prev := forgeReadWait
			forgeReadWait = 300 * time.Millisecond
			t.Cleanup(func() { forgeReadWait = prev })
			f.apiHook = func(w http.ResponseWriter, r *http.Request) bool {
				select {
				case <-time.After(5 * time.Second):
				case <-r.Context().Done():
				}
				return true
			}
		}, "deadline exceeded"},
		{"the comparison needs more reads than a push may make", func(t *testing.T, f *gitForge) {
			prev := maxForgeReads
			maxForgeReads = 1
			t.Cleanup(func() { maxForgeReads = prev })
		}, "more than 1 reads"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newForgeRun(t, ordinaryRepo, []string{"--depth", "1"}, ".github/workflows/**")
			c.setup(t, r.forge)
			r.editMain(t, "blind")
			out, err := r.push()
			r.wantRefusedFor(t, out, err, ".github/workflows/ci.yml", c.why)
			if !strings.Contains(r.lastRefusal(), "  .github/\n") {
				t.Errorf("refusal %q does not name the directory it could not compare", r.lastRefusal())
			}
		})
	}
	t.Run("a forge other than GitHub", func(t *testing.T) {
		r := newForgeRunVia(t, "gitlab.com", ordinaryRepo, []string{"--depth", "1"}, ".github/workflows/**")
		r.editMain(t, "elsewhere")
		out, err := r.push()
		r.wantRefusedFor(t, out, err, ".github/workflows/ci.yml", whyNotGitHub)
		if n, _ := r.forge.reads(); n != 0 {
			t.Errorf("a push to a forge that is not GitHub read the GitHub API %d time(s)", n)
		}
	})
}
