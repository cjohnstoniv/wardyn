// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ─── fixtures built by real git ─────────────────────────────────────────────
//
// Every non-hostile fixture in this file is a receive-pack request body that a
// real `git push` produced. The trick is --receive-pack: git speaks the same
// pkt-line command section and packfile to whatever that names, over the local
// transport as over HTTP, so a wrapper that tees its stdin captures byte for
// byte what an HTTP push would carry in its body. A pack hand-assembled by the
// test would only prove the parser agrees with the test's idea of the format.

// requireGit skips the calling test — with a reason that names what is missing —
// when git is not installed, the convention TestRunFilesScript_* follows.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH: the fixtures in this package are built by real git")
	}
}

// repo is a work tree, a bare remote to push at, and the capture script.
type repo struct {
	t      *testing.T
	work   string
	remote string
	script string
	bodyAt string
}

func newRepo(t *testing.T, initArgs ...string) *repo {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	r := &repo{
		t:      t,
		work:   filepath.Join(dir, "work"),
		remote: filepath.Join(dir, "remote.git"),
		script: filepath.Join(dir, "capture.sh"),
		bodyAt: filepath.Join(dir, "body.bin"),
	}
	script := "#!/bin/sh\ntee \"" + r.bodyAt + "\" | git receive-pack \"$@\"\n"
	if err := os.WriteFile(r.script, []byte(script), 0o755); err != nil {
		t.Fatalf("write capture script: %v", err)
	}
	r.runIn(dir, append(append([]string{"init", "-q", "--bare"}, initArgs...), r.remote)...)
	r.runIn(dir, append(append([]string{"init", "-q"}, initArgs...), r.work)...)
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test")
	return r
}

func (r *repo) runIn(dir string, args ...string) string {
	r.t.Helper()
	return r.feed(dir, "", args...)
}

// feed is runIn with something on stdin, for the plumbing that reads an object
// body there: `git mktree` and the `git hash-object` that writes the commit
// shapes `git commit-tree` refuses to produce.
func (r *repo) feed(dir, stdin string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_DATE=@1767225600 +0000", "GIT_COMMITTER_DATE=@1767225600 +0000")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) git(args ...string) string { r.t.Helper(); return r.runIn(r.work, args...) }

func (r *repo) gitStdin(stdin string, args ...string) string {
	r.t.Helper()
	return r.feed(r.work, stdin, args...)
}

// write puts one file in the work tree and stages it.
func (r *repo) write(path, content string, mode os.FileMode) {
	r.t.Helper()
	full := filepath.Join(r.work, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		r.t.Fatalf("write %s: %v", path, err)
	}
	r.git("add", "--", path) // git reads the executable bit off the filesystem
}

// commit records what write and the explicit index edits have staged. It does
// NOT run `git add -A`: that would drop the gitlink entry the modes test stages
// by hand, since no such directory exists in the work tree.
func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// push runs a real push at the capture script and returns the request body git sent.
func (r *repo) push(refspec string, extra ...string) []byte {
	r.t.Helper()
	args := append([]string{"push", "-q"}, extra...)
	args = append(args, "--receive-pack="+r.script, r.remote, refspec)
	r.git(args...)
	body, err := os.ReadFile(r.bodyAt)
	if err != nil {
		r.t.Fatalf("read captured body: %v", err)
	}
	return body
}

// ─── assertions ─────────────────────────────────────────────────────────────

// paths renders a change set as one "path mode size" line per change, in the
// order Inspect returned it.
func paths(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, fmt.Sprintf("%s %s %d", c.Path, c.Mode, c.Size))
	}
	return out
}

func wantChanges(t *testing.T, got []Change, want ...string) {
	t.Helper()
	if g := strings.Join(paths(got), "\n"); g != strings.Join(want, "\n") {
		t.Fatalf("changes:\ngot:\n%s\nwant:\n%s", g, strings.Join(want, "\n"))
	}
}

// ─── the change set ─────────────────────────────────────────────────────────

// TestPackInspect_SelfContainedPackReportsChangedPaths pins the whole answer for
// an ordinary second push, INCLUDING both documented over-reports:
// dir/unchanged.txt is reported because its parent directory changed and the
// pre-image tree is not in the pack, and keep/ is reported as ONE uncarried
// directory — the receiving side stores its tree, but nothing in the pack says
// that tree stood at keep/ before this push.
func TestPackInspect_SelfContainedPackReportsChangedPaths(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "a\n", 0o644)
	r.write("dir/b.txt", "b\n", 0o644)
	r.write("dir/unchanged.txt", "still here\n", 0o644)
	r.write("keep/u.txt", "untouched\n", 0o644)
	r.commit("one")
	r.push("HEAD:refs/heads/main", "--no-thin")

	r.write("dir/b.txt", "b two\n", 0o644)
	r.write("c.txt", "c\n", 0o644)
	r.git("rm", "-q", "--", "a.txt")
	second := r.commit("two")
	body := r.push("HEAD:refs/heads/wardyn/run/work", "--no-thin")

	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if res.ObjectFormat != "sha1" {
		t.Errorf("ObjectFormat = %q, want sha1", res.ObjectFormat)
	}
	if len(res.Commands) != 1 || res.Commands[0].New != second ||
		res.Commands[0].Ref != "refs/heads/wardyn/run/work" {
		t.Errorf("Commands = %+v, want one update of refs/heads/wardyn/run/work to %s", res.Commands, second)
	}
	wantChanges(t, res.Changes,
		"c.txt 100644 2",
		"dir/b.txt 100644 6",
		"dir/unchanged.txt 100644 -1",
		"keep "+ModeUncarried+" -1",
	)
}

// TestPackInspect_FirstPushEnumeratesWholeTree is the over-reporting case the
// package documents: no parent is in the pack, so every path the commit holds is
// reported, not only what an operator would call "changed".
func TestPackInspect_FirstPushEnumeratesWholeTree(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "a\n", 0o644)
	r.write("dir/b.txt", "b\n", 0o644)
	r.write("keep/u.txt", "untouched\n", 0o644)
	r.commit("one")
	body := r.push("HEAD:refs/heads/main", "--no-thin")

	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	wantChanges(t, res.Changes,
		"a.txt 100644 2",
		"dir/b.txt 100644 2",
		"keep/u.txt 100644 10",
	)
}

// TestPackInspect_ModesSurfaceVerbatim keeps the three modes a content rule cares
// about distinguishable: an executable, a symlink and a submodule pointer.
func TestPackInspect_ModesSurfaceVerbatim(t *testing.T) {
	r := newRepo(t)
	r.write("plain.txt", "plain\n", 0o644)
	r.write("tool.sh", "#!/bin/sh\n", 0o755)
	if err := os.Symlink("plain.txt", filepath.Join(r.work, "link")); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}
	r.git("add", "--", "link")
	// A gitlink without a real submodule checkout: the entry is what matters,
	// and the commit it names lives in the other repository by design.
	r.git("update-index", "--add", "--cacheinfo",
		"160000,1111111111111111111111111111111111111111,sub")
	r.commit("one")
	body := r.push("HEAD:refs/heads/main", "--no-thin")

	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	wantChanges(t, res.Changes,
		"link 120000 9",
		"plain.txt 100644 6",
		"sub 160000 -1",
		"tool.sh 100755 10",
	)
}

// TestPackInspect_ResolvesAnOffsetDeltaAgainstItsBase pushes two near-identical
// files, which git deltifies inside one pack (OBJ_OFS_DELTA). The proof is the
// SIZE: a delta applied wrongly yields a plausible path with a wrong size, so
// the sizes are what the assertion pins.
func TestPackInspect_ResolvesAnOffsetDeltaAgainstItsBase(t *testing.T) {
	r := newRepo(t)
	var one, two strings.Builder
	for i := range 300 {
		fmt.Fprintf(&one, "line %d of a file long enough to deltify\n", i)
		if i == 5 {
			fmt.Fprintf(&two, "CHANGED\n")
			continue
		}
		fmt.Fprintf(&two, "line %d of a file long enough to deltify\n", i)
	}
	r.write("one.txt", one.String(), 0o644)
	r.write("two.txt", two.String(), 0o644)
	r.commit("one")
	body := r.push("HEAD:refs/heads/main", "--no-thin")

	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	wantChanges(t, res.Changes,
		fmt.Sprintf("one.txt 100644 %d", one.Len()),
		fmt.Sprintf("two.txt 100644 %d", two.Len()),
	)
}

// ─── the refusals ───────────────────────────────────────────────────────────

// TestPackInspect_ThinPackIsUninspectable is the first-class refusal: --thin
// deltas against a base that lives only on the receiving side, and chasing it
// would mean putting the proxy on the network for the run.
func TestPackInspect_ThinPackIsUninspectable(t *testing.T) {
	r := newRepo(t)
	var big strings.Builder
	for i := range 400 {
		fmt.Fprintf(&big, "line %d of a file long enough to deltify\n", i)
	}
	r.write("big.txt", big.String(), 0o644)
	r.commit("one")
	r.push("HEAD:refs/heads/main", "--no-thin")

	r.write("big.txt", big.String()+"one more line\n", 0o644)
	r.commit("two")
	body := r.push("HEAD:refs/heads/thin", "--thin")

	res, err := Inspect(body)
	if !errors.Is(err, ErrUninspectable) {
		t.Fatalf("Inspect err = %v, want ErrUninspectable", err)
	}
	if len(res.Changes) != 0 {
		t.Errorf("Changes = %v, want none alongside a refusal", res.Changes)
	}
}

// TestPackInspect_TruncatedPackErrors: a short read must be an error, never a
// partial answer a deny rule would then evaluate as if it were the whole push.
func TestPackInspect_TruncatedPackErrors(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", strings.Repeat("a\n", 500), 0o644)
	r.write("dir/b.txt", "b\n", 0o644)
	r.commit("one")
	body := r.push("HEAD:refs/heads/main", "--no-thin")

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"trailer cut", body[:len(body)-20]},
		{"mid-pack cut", body[:len(body)-120]},
		{"header only", body[:strings.Index(string(body), "PACK")+8]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Inspect(tc.body)
			if err == nil {
				t.Fatalf("Inspect returned %d changes and no error on a truncated pack", len(res.Changes))
			}
			if len(res.Changes) != 0 {
				t.Errorf("Changes = %v, want none alongside an error", res.Changes)
			}
		})
	}
}

// TestPackInspect_NewRefWithoutItsCommitIsUninspectable covers the push that
// creates a ref at an object the receiving side already has: the pack is empty,
// so nothing about that ref's content was inspected and the answer must say so.
func TestPackInspect_NewRefWithoutItsCommitIsUninspectable(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "a\n", 0o644)
	r.commit("one")
	r.push("HEAD:refs/heads/main", "--no-thin")
	body := r.push("HEAD:refs/heads/second", "--no-thin")

	if _, err := Inspect(body); !errors.Is(err, ErrUninspectable) {
		t.Fatalf("Inspect err = %v, want ErrUninspectable", err)
	}
}

// TestPackInspect_DeleteOnlyPushHasNothingToInspect: a ref deletion carries no
// pack, and that is an answer, not a refusal.
func TestPackInspect_DeleteOnlyPushHasNothingToInspect(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "a\n", 0o644)
	r.commit("one")
	r.push("HEAD:refs/heads/doomed", "--no-thin")
	body := r.push(":refs/heads/doomed")

	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(res.Changes) != 0 || len(res.Commands) != 1 || res.Commands[0].Ref != "refs/heads/doomed" {
		t.Fatalf("res = %+v, want the one delete command and no changes", res)
	}
}

// TestPackInspect_DuplicateTreeHeaderIsRefused is the disagreement that must
// never be answered: git's commit parser takes the FIRST tree header, so a
// second one placed after the committer is a tree only a last-wins reader sees.
// The benign tree it names is not in the pack — nothing reaches it — so a
// last-wins reader treats it as already present on the receiving side and
// reports no change at all, while git checks the evil tree out. `git fsck` is
// clean on this object and a remote with receive.fsckObjects=true takes it, so
// only the inspector can refuse it.
//
// It is assembled with `git hash-object -t commit`: `git commit-tree` will not
// emit a second tree header.
func TestPackInspect_DuplicateTreeHeaderIsRefused(t *testing.T) {
	r := newRepo(t)
	benignBlob := r.gitStdin("benign\n", "hash-object", "-w", "-t", "blob", "--stdin")
	evilBlob := r.gitStdin("SECRET=1\n", "hash-object", "-w", "-t", "blob", "--stdin")
	benignTree := r.gitStdin("100644 blob "+benignBlob+"\treadme.md\n", "mktree")
	evilTree := r.gitStdin("100644 blob "+evilBlob+"\tsecrets.env\n", "mktree")
	commit := r.gitStdin("tree "+evilTree+"\n"+
		"author T <t@example.com> 1767225600 +0000\n"+
		"committer T <t@example.com> 1767225600 +0000\n"+
		"tree "+benignTree+"\n\nx\n",
		"hash-object", "-w", "-t", "commit", "--stdin")

	// What the receiving side would store, so the fixture cannot rot into a
	// commit git reads the same way this package does.
	if got := r.git("rev-parse", commit+"^{tree}"); got != evilTree {
		t.Fatalf("git resolves the commit to tree %s, want the first header %s", got, evilTree)
	}
	r.git("update-ref", "refs/heads/evil", commit)
	body := r.push("refs/heads/evil")

	res, err := Inspect(body)
	if !errors.Is(err, ErrUninspectable) {
		t.Fatalf("Inspect = (%v, %v), want ErrUninspectable", paths(res.Changes), err)
	}
	if len(res.Changes) != 0 {
		t.Errorf("Changes = %v, want none alongside an error", paths(res.Changes))
	}
}

// TestPackCommit_HeadersGitWouldNotReadAreRefused covers the same class at the
// parser: git stops its parent loop at the first header that is not a parent,
// so a parent line below the committer is one git never reads either.
func TestPackCommit_HeadersGitWouldNotReadAreRefused(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	ident := "author T <t@example.com> 1767225600 +0000\n" +
		"committer T <t@example.com> 1767225600 +0000\n"
	for _, tc := range []struct {
		name, obj string
		refused   bool
	}{
		{"one tree and its parents", "tree " + a + "\nparent " + b + "\n" + ident + "\nok\n", false},
		{"a second tree below the committer", "tree " + a + "\n" + ident + "tree " + b + "\n\nx\n", true},
		{"a second tree above the committer", "tree " + a + "\ntree " + b + "\n" + ident + "\nx\n", true},
		{"a parent below the committer", "tree " + a + "\n" + ident + "parent " + b + "\n\nx\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCommit([]byte(tc.obj), 20)
			switch {
			case tc.refused && !errors.Is(err, ErrUninspectable):
				t.Fatalf("parseCommit = (%+v, %v), want ErrUninspectable", got, err)
			case !tc.refused && err != nil:
				t.Fatalf("parseCommit: %v", err)
			case !tc.refused && (got.tree != a || len(got.parents) != 1):
				t.Fatalf("parseCommit = %+v, want tree %s and one parent", got, a)
			}
		})
	}
}

// TestPackInspect_DirectoryModeIsMaskedLikeGit: git's test for a directory is
// S_ISDIR — the FORMAT bits of the mode, masked — so every spelling below whose
// format bits are 0o040000 is a directory it recurses into. A check that
// matched the string "40000", or compared the whole parsed mode for equality,
// read a leaf instead and lost every path beneath it.
//
// The fsck severities are what leave the inspector alone with this: strict
// receive.fsckObjects REJECTS "040000" (zeroPaddedFilemode, an error) and
// ACCEPTS "40001" (badFilemode, only a warning), so for every spelling but the
// zero-padded one there is no backstop behind this check.
//
// "140000" is the control: those format bits are not S_IFDIR, git reads a leaf
// there, and so must this.
func TestPackInspect_DirectoryModeIsMaskedLikeGit(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"40000", "config/secrets.env 100644 9"},
		{"040000", "config/secrets.env 100644 9"},
		{"40001", "config/secrets.env 100644 9"},
		{"40644", "config/secrets.env 100644 9"},
		{"47777", "config/secrets.env 100644 9"},
		{"140000", "config 140000 -1"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			blob := []byte("SECRET=1\n")
			sub := mkTree(treeLine{"100644", "secrets.env", hashObject("blob", blob)})
			root := mkTree(treeLine{tc.mode, "config", hashObject("tree", sub)})
			commit := mkCommit(hashObject("tree", root))
			pack := buildPack(t,
				rawObject{typ: objBlob, payload: blob},
				rawObject{typ: objTree, payload: sub},
				rawObject{typ: objTree, payload: root},
				rawObject{typ: objCommit, payload: commit},
			)
			zero := strings.Repeat("0", 40)
			body := append(commandSection("report-status object-format=sha1",
				zero+" "+hashObject("commit", commit)+" refs/heads/main"), pack...)

			res, err := Inspect(body)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			wantChanges(t, res.Changes, tc.want)
		})
	}
}

// ─── the wire ───────────────────────────────────────────────────────────────

// TestPackInspect_SkipsThePushOptionsSection: `git push -o` puts a second
// pkt-line section between the commands and the pack. Reading it as pack bytes
// would refuse a legitimate push.
func TestPackInspect_SkipsThePushOptionsSection(t *testing.T) {
	r := newRepo(t)
	r.runIn(r.remote, "config", "receive.advertisePushOptions", "true")
	r.write("a.txt", "a\n", 0o644)
	r.commit("one")
	body := r.push("HEAD:refs/heads/main", "--no-thin", "-o", "ticket=42")

	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	wantChanges(t, res.Changes, "a.txt 100644 2")
}

// TestPackInspect_ObjectFormatSHA256 reads a pack whose object ids are 32 bytes
// wide, because the capability list said so.
func TestPackInspect_ObjectFormatSHA256(t *testing.T) {
	requireGit(t)
	r := newRepo(t, "--object-format=sha256")
	r.write("a.txt", "a\n", 0o644)
	r.write("dir/b.txt", "b\n", 0o644)
	head := r.commit("one")
	if len(head) != 64 {
		t.Skipf("this git does not build sha256 repositories (HEAD = %q)", head)
	}
	body := r.push("HEAD:refs/heads/main", "--no-thin")

	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if res.ObjectFormat != "sha256" {
		t.Errorf("ObjectFormat = %q, want sha256", res.ObjectFormat)
	}
	wantChanges(t, res.Changes, "a.txt 100644 2", "dir/b.txt 100644 2")
}

// TestPackInspect_UnknownObjectFormatIsRefused: guessing a hash width would mean
// reading every object id at the wrong offset and reporting whatever fell out.
func TestPackInspect_UnknownObjectFormatIsRefused(t *testing.T) {
	zero := strings.Repeat("0", 40)
	one := strings.Repeat("1", 40)
	body := commandSection("report-status object-format=sha512 agent=git/2.43.0",
		zero+" "+one+" refs/heads/main")
	_, err := Inspect(body)
	if err == nil || !strings.Contains(err.Error(), "sha512") {
		t.Fatalf("Inspect err = %v, want a refusal naming sha512", err)
	}
}

// ─── the delta applier ──────────────────────────────────────────────────────
//
// The dangerous failure is an applier that quietly produces a short or over-long
// buffer: the object then hashes to nothing the tree names, the path is still
// reported, and a test that only checks "we got some paths" stays green while the
// answer is wrong. Every case below is a size the delta header declared and the
// instructions did not deliver.

func TestPackDelta_AppliesACorrectDelta(t *testing.T) {
	base := []byte("the quick brown fox\n")
	want := []byte("the quick red fox\n")
	d := deltaStream(int64(len(base)), int64(len(want)),
		copyInstr(0, 10), insertInstr([]byte("red fox\n")))
	got, err := applyDelta(base, d)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("applyDelta = %q, want %q", got, want)
	}
}

func TestPackDelta_SizeMismatchFailsClosed(t *testing.T) {
	base := []byte("the quick brown fox\n")
	for _, tc := range []struct {
		name  string
		delta []byte
	}{
		{"result shorter than declared", deltaStream(int64(len(base)), 99,
			copyInstr(0, 10), insertInstr([]byte("red fox\n")))},
		{"result longer than declared", deltaStream(int64(len(base)), 3,
			copyInstr(0, 10), insertInstr([]byte("red fox\n")))},
		{"base size not this base", deltaStream(int64(len(base))+1, 10, copyInstr(0, 10))},
		{"copy runs past the base", deltaStream(int64(len(base)), 10, copyInstr(15, 10))},
		{"copy offset past the base", deltaStream(int64(len(base)), 4, copyInstr(1<<20, 4))},
		{"insert runs off the end", append(deltaStream(int64(len(base)), 4), 0x04, 'a', 'b')},
		{"zero opcode is not an instruction", deltaStream(int64(len(base)), 1, []byte{0x00})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyDelta(base, tc.delta)
			if err == nil {
				t.Fatalf("applyDelta returned %q and no error", got)
			}
			if got != nil {
				t.Errorf("applyDelta returned %q alongside an error", got)
			}
		})
	}
}

// TestPackInspect_LyingDeltaFailsClosed is the same failure end to end, in the
// two shapes it takes: a delta that declares a size its instructions do not
// deliver, and instructions that deliver a size the delta did not declare. The
// pack's tree names the object id the HONEST content would hash to, so with the
// assertion removed Inspect answers with a plausible change set — a real path at
// a real mode — and the wrong size beside it.
func TestPackInspect_LyingDeltaFailsClosed(t *testing.T) {
	blob := []byte("the quick brown fox\n")
	honest := []byte("the quick red fox\n")
	for _, tc := range []struct {
		name  string
		delta []byte
	}{
		{"declares one byte more than it delivers", deltaStream(int64(len(blob)), int64(len(honest))+1,
			copyInstr(0, 10), insertInstr([]byte("red fox\n")))},
		{"delivers one byte less than it declares", deltaStream(int64(len(blob)), int64(len(honest)),
			copyInstr(0, 10), insertInstr([]byte("red fox")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := mkTree(treeLine{"100644", "f.txt", hashObject("blob", honest)})
			commit := mkCommit(hashObject("tree", tree))
			pack := buildPack(t,
				rawObject{typ: objBlob, payload: blob},
				rawObject{typ: objOfsDelta, payload: tc.delta,
					backTo: len(objHeader(objBlob, int64(len(blob)))) + deflatedLen(t, blob)},
				rawObject{typ: objTree, payload: tree},
				rawObject{typ: objCommit, payload: commit},
			)
			zero := strings.Repeat("0", 40)
			body := append(commandSection("report-status object-format=sha1",
				zero+" "+hashObject("commit", commit)+" refs/heads/main"), pack...)

			res, err := Inspect(body)
			if err == nil {
				t.Fatalf("Inspect accepted a lying delta and answered %v", paths(res.Changes))
			}
			if !strings.Contains(err.Error(), "size") {
				t.Errorf("err = %v, want the size mismatch named", err)
			}
			if len(res.Changes) != 0 {
				t.Errorf("Changes = %v, want none alongside an error", res.Changes)
			}
		})
	}
}

// ─── the tree diff ──────────────────────────────────────────────────────────

// TestPackTree_DiffReportsOnlyWhatChanged pins the diff directly. It is not
// observable through Inspect: the oldest new commit in any pack has no
// pre-image, so its whole-tree enumeration already covers every path the diff
// would report for the commits above it.
func TestPackTree_DiffReportsOnlyWhatChanged(t *testing.T) {
	blobA, blobB, blobC := []byte("a\n"), []byte("b\n"), []byte("b two\n")
	oldSub := mkTree(treeLine{"100644", "x.txt", hashObject("blob", blobA)},
		treeLine{"100644", "y.txt", hashObject("blob", blobB)})
	newSub := mkTree(treeLine{"100644", "x.txt", hashObject("blob", blobA)},
		treeLine{"100644", "y.txt", hashObject("blob", blobC)})
	oldRoot := mkTree(treeLine{"40000", "dir", hashObject("tree", oldSub)})
	newRoot := mkTree(treeLine{"40000", "dir", hashObject("tree", newSub)})

	idx := newIndex(sha1Format)
	for typ, payload := range map[objectType][][]byte{
		objBlob: {blobA, blobB, blobC},
		objTree: {oldSub, newSub, oldRoot, newRoot},
	} {
		for _, p := range payload {
			idx.put(typ, p)
		}
	}
	w := newWalker(idx)
	if err := w.diff("", hashObject("tree", oldRoot), hashObject("tree", newRoot), 0); err != nil {
		t.Fatalf("diff: %v", err)
	}
	wantChanges(t, w.out, "dir/y.txt 100644 6")
}

// TestPackTree_UncarriedDirectoryIsReportedNotSkipped pins what a subtree the
// pack does not carry becomes. A pack leaves out whatever the receiving side
// stores, wherever the new tree puts it, so an absent tree is the same bytes
// whether it was left alone, moved onto a denied path, or restored whole from
// an older revision. Skipping it let any of those carry anything anywhere;
// it is reported as one opaque entry at its own path instead. The one exact
// skip is a diff against a parent the pack carries, where the same object id
// at the same name proves the directory untouched.
func TestPackTree_UncarriedDirectoryIsReportedNotSkipped(t *testing.T) {
	forgeHeld := strings.Repeat("ab", 20) // a tree the pack does not carry
	elsewhere := strings.Repeat("cd", 20)

	t.Run("enumerated: an absent subtree is one uncarried directory", func(t *testing.T) {
		idx := newIndex(sha1Format)
		root := idx.put(objTree, mkTree(treeLine{"40000", "infra", forgeHeld}))
		w := newWalker(idx)
		if err := w.walk("", root, 0); err != nil {
			t.Fatalf("walk: %v", err)
		}
		wantChanges(t, w.out, "infra "+ModeUncarried+" -1")
	})
	t.Run("enumerated: a whole root tree the pack does not carry", func(t *testing.T) {
		w := newWalker(newIndex(sha1Format))
		if err := w.walk("", forgeHeld, 0); err != nil {
			t.Fatalf("walk: %v", err)
		}
		if len(w.out) != 1 || w.out[0].Path != "" || w.out[0].Mode != ModeUncarried {
			t.Fatalf("Changes = %+v, want the root reported as one uncarried directory", w.out)
		}
	})
	t.Run("diffed: a subtree swapped for one the pack does not carry", func(t *testing.T) {
		idx := newIndex(sha1Format)
		oldRoot := idx.put(objTree, mkTree(treeLine{"40000", "infra", elsewhere}))
		newRoot := idx.put(objTree, mkTree(treeLine{"40000", "infra", forgeHeld}))
		w := newWalker(idx)
		if err := w.diff("", oldRoot, newRoot, 0); err != nil {
			t.Fatalf("diff: %v", err)
		}
		wantChanges(t, w.out, "infra "+ModeUncarried+" -1")
	})
	t.Run("diffed: an unchanged subtree is skipped exactly", func(t *testing.T) {
		blob := []byte("x\n")
		idx := newIndex(sha1Format)
		idx.put(objBlob, blob)
		oldRoot := idx.put(objTree, mkTree(treeLine{"40000", "infra", forgeHeld}))
		newRoot := idx.put(objTree, mkTree(treeLine{"100644", "a.txt", hashObject("blob", blob)},
			treeLine{"40000", "infra", forgeHeld}))
		w := newWalker(idx)
		if err := w.diff("", oldRoot, newRoot, 0); err != nil {
			t.Fatalf("diff: %v", err)
		}
		wantChanges(t, w.out, "a.txt 100644 2")
	})
}

// TestPackSettle_HistoryTheReceiverHoldsIntroducesNothing: a sender holding
// none of the tips the receiving side advertises re-sends its history, and
// Inspect enumerates that history's first commit whole. Settle takes out what
// the caller says the receiving side holds, diffs what is left against it,
// and asks as few questions as it can.
func TestPackSettle_HistoryTheReceiverHoldsIntroducesNothing(t *testing.T) {
	r := newRepo(t)
	r.write(".github/ci.yml", "on: push\n", 0o644)
	r.write("src/a.go", "a\n", 0o644)
	root := r.commit("root")
	r.write("src/a.go", "a two\n", 0o644)
	base := r.commit("base")
	r.write("src/a.go", "a three\n", 0o644)
	mid := r.commit("mid")
	r.write("src/b.go", "b\n", 0o644)
	r.commit("tip")
	// The remote is empty and advertises nothing, so all four commits go.
	res, err := Inspect(r.push("HEAD:refs/heads/work", "--no-thin"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	wantChanges(t, res.Changes, ".github/ci.yml 100644 9", "src/a.go 100644 2", "src/a.go 100644 6",
		"src/a.go 100644 8", "src/b.go 100644 2")

	settle := func(t *testing.T, holds ...string) (Result, []string) {
		t.Helper()
		var asked []string
		out, err := res.Settle(func(c string) (bool, error) {
			asked = append(asked, c)
			return slices.Contains(holds, c), nil
		})
		if err != nil {
			t.Fatalf("Settle: %v", err)
		}
		return out, asked
	}
	t.Run("the receiving side holds the history beneath the push", func(t *testing.T) {
		out, asked := settle(t, root, base)
		wantChanges(t, out.Changes, "src/a.go 100644 8", "src/b.go 100644 2")
		if !slices.Equal(out.Bases, []string{base}) {
			t.Errorf("Bases = %v, want the held commit the push builds on, %s", out.Bases, base)
		}
		// The bottom first, then down from the tip until a held commit answers
		// for the rest. The tip itself is never asked about.
		if want := []string{root, mid, base}; !slices.Equal(asked, want) {
			t.Errorf("asked about %v, want %v", asked, want)
		}
	})
	t.Run("the receiving side holds none of it", func(t *testing.T) {
		out, asked := settle(t)
		wantChanges(t, out.Changes, paths(res.Changes)...)
		if want := []string{root}; !slices.Equal(asked, want) {
			t.Errorf("asked about %v, want only %v: nothing above a new commit is held", asked, want)
		}
	})
}

// TestPackChange_OpaqueIsEverythingButARegularFile: git checks out a mode it
// cannot classify as a submodule pointer, so anything that is not a regular
// file — and anything that does not parse — may stand for paths beneath it.
func TestPackChange_OpaqueIsEverythingButARegularFile(t *testing.T) {
	for mode, want := range map[string]bool{
		"100644": false, "100755": false, "100664": false,
		"120000": true, "160000": true, ModeUncarried: true, "10644": true, "": true, "x": true,
	} {
		if got := (Change{Mode: mode}).Opaque(); got != want {
			t.Errorf("Change{Mode: %q}.Opaque() = %v, want %v", mode, got, want)
		}
	}
}

// ─── the walk's ceilings ────────────────────────────────────────────────────
//
// Real git cannot build these: every one is a tree object naming another tree
// object that was never written to describe a directory.

// fanOut stacks levels of trees, each naming the level below under every name
// given, and returns the root. A DAG, not a tree: one object per level, b^d
// paths through it.
func fanOut(idx *index, bottom []byte, levels int, names ...string) string {
	oid := idx.put(objTree, bottom)
	for range levels {
		lines := make([]treeLine, 0, len(names))
		for _, n := range names {
			lines = append(lines, treeLine{"40000", n, oid})
		}
		oid = idx.put(objTree, mkTree(lines...))
	}
	return oid
}

// absentSubtrees is a tree whose every entry names a subtree the pack does not
// carry, so each is reported as one uncarried directory.
func absentSubtrees(n int) []byte {
	absent := strings.Repeat("0", 40)
	lines := make([]treeLine, 0, n)
	for i := range n {
		lines = append(lines, treeLine{"40000", fmt.Sprintf("s%04d", i), absent})
	}
	return mkTree(lines...)
}

// emptySubtrees is a tree whose every entry names the empty tree, which the
// pack carries: a fan-out that bottoms out here resolves to no Change at all,
// so maxChanges is never reached and only the node ceiling can stop the walk.
// It returns the tree and the empty tree's object id.
func emptySubtrees(idx *index, n int) (tree []byte, emptyOID string) {
	emptyOID = idx.put(objTree, mkTree())
	lines := make([]treeLine, 0, n)
	for i := range n {
		lines = append(lines, treeLine{"40000", fmt.Sprintf("s%04d", i), emptyOID})
	}
	return mkTree(lines...), emptyOID
}

// TestPackTree_FanOutDAGIsChargedAgainstMaxTreeNodes is the cheapest denial of
// service the format allows: 65 tree objects, 3 KB, and 2^64 expansions if the
// walk follows every path. maxTreeDepth is satisfied the whole way down,
// because depth is not what is unbounded here.
//
// The wide case is the same attack with the bottom widened. It is the reason
// the ceiling counts tree ENTRIES: charging one unit per expansion made width
// free, so the same ceiling admitted hundreds of times the work — a larger body
// stopped at the identical node count after minutes rather than a second.
//
// The assertion is the work done, not the time taken: a wall clock would only
// say this machine was fast today.
func TestPackTree_FanOutDAGIsChargedAgainstMaxTreeNodes(t *testing.T) {
	for _, width := range []int{2, 256} {
		t.Run(fmt.Sprintf("bottom of %d", width), func(t *testing.T) {
			idx := newIndex(sha1Format)
			bottom, emptyOID := emptySubtrees(idx, width)
			// One level short of the depth ceiling: the empty trees sit a level
			// below the bottom, and depth is not what this test is about.
			root := fanOut(idx, bottom, maxTreeDepth-1, "a", "b")

			w := newWalker(idx)
			err := w.walk("", root, 0)
			if !errors.Is(err, ErrUninspectable) {
				t.Fatalf("walk = %v, want ErrUninspectable", err)
			}
			// One charge covers a whole tree, so the count may overshoot by at
			// most the widest tree in the pack — never by a multiple of it.
			if w.nodes > maxTreeNodes+width {
				t.Errorf("walked %d entries, want the walk stopped at %d", w.nodes, maxTreeNodes)
			}
			// Every tree here but the empty one holds at least two entries, so a
			// ceiling that bounds WORK cannot have admitted more than half its
			// budget in expansions of them. Charging a flat unit per expansion
			// satisfies the count above while doing `width` times the lookups
			// underneath it.
			trees := 0
			for key := range w.walked {
				if !strings.HasSuffix(key, emptyOID) {
					trees++
				}
			}
			if 2*trees > w.nodes {
				t.Errorf("expanded %d trees of >=2 entries each but charged %d: width is not charged",
					trees, w.nodes)
			}
			if len(w.out) != 0 {
				t.Errorf("Changes = %v, want none", paths(w.out))
			}
		})
	}
}

// TestPackTree_WidthIsChargedNotJustDepth: a tree's entries each cost a lookup
// whether or not the pack carries what they name, so one wide-and-shallow tree
// is charged for its width. Charging a flat unit per expansion left that work
// outside every ceiling.
func TestPackTree_WidthIsChargedNotJustDepth(t *testing.T) {
	const width = 4096
	idx := newIndex(sha1Format)
	root := idx.put(objTree, absentSubtrees(width))

	w := newWalker(idx)
	if err := w.walk("", root, 0); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if w.nodes != width {
		t.Errorf("walked %d entries, want %d — the tree's width", w.nodes, width)
	}
	// Each absent subtree is one uncarried directory, never a silent skip.
	if len(w.out) != width || !w.out[0].Opaque() || w.out[0].Mode != ModeUncarried {
		t.Errorf("Changes = %d, first %+v; want %d uncarried directories", len(w.out), w.out[0], width)
	}
}

// TestPackTree_RepeatedSubtreeIsExpandedOnce pins the memo that makes the
// common shape cheap without changing the answer: a subtree reached by two
// paths is expanded once per path, never twice per path, and still contributes
// its leaves under BOTH prefixes — skipping the second prefix would
// under-report, which is the unsafe direction.
func TestPackTree_RepeatedSubtreeIsExpandedOnce(t *testing.T) {
	blob := []byte("x\n")
	sub := mkTree(treeLine{"100644", "f.txt", hashObject("blob", blob)})
	idx := newIndex(sha1Format)
	idx.put(objBlob, blob)
	subOID := idx.put(objTree, sub)
	root := idx.put(objTree, mkTree(
		treeLine{"40000", "one", subOID}, treeLine{"40000", "two", subOID}))

	w := newWalker(idx)
	if err := w.walk("", root, 0); err != nil {
		t.Fatalf("walk: %v", err)
	}
	wantChanges(t, w.out, "one/f.txt 100644 2", "two/f.txt 100644 2")
	// The root's two entries, then the subtree's one entry under each of the two
	// prefixes it is reached by.
	const want = 2 + 1 + 1
	if w.nodes != want {
		t.Errorf("walked %d entries, want %d", w.nodes, want)
	}
	// The memo, which is the whole point: the second walk charges nothing.
	if err := w.walk("", root, 0); err != nil || w.nodes != want {
		t.Errorf("re-walking the same root: err=%v, walked %d entries, want %d", err, w.nodes, want)
	}
}

// TestPackTree_MaxTreeDepthIsEnforced pins the nesting ceiling from both sides,
// so it stays where the constant says it is.
func TestPackTree_MaxTreeDepthIsEnforced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		levels  int
		refused bool
	}{
		{"at the ceiling", maxTreeDepth, false},
		{"one level past it", maxTreeDepth + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob := []byte("x\n")
			idx := newIndex(sha1Format)
			idx.put(objBlob, blob)
			root := fanOut(idx, mkTree(treeLine{"100644", "f.txt", hashObject("blob", blob)}),
				tc.levels, "d")

			w := newWalker(idx)
			err := w.walk("", root, 0)
			if !tc.refused {
				if err != nil {
					t.Fatalf("walk: %v", err)
				}
				wantChanges(t, w.out, strings.Repeat("d/", tc.levels)+"f.txt 100644 2")
				return
			}
			if err == nil || !strings.Contains(err.Error(), "nested deeper") {
				t.Fatalf("walk = %v, want the depth ceiling named", err)
			}
		})
	}
}

// TestPackTree_MaxChangesIsEnforced pins the change-set ceiling. 16 levels of
// two-way fan-out over a four-entry bottom describe 262,144 distinct paths in
// 17 tree objects, so the walk stops with the ceiling's own refusal rather than
// on the node ceiling or a truncated answer.
func TestPackTree_MaxChangesIsEnforced(t *testing.T) {
	idx := newIndex(sha1Format)
	absent := strings.Repeat("0", 40)
	root := fanOut(idx, mkTree(
		treeLine{"100644", "f0", absent}, treeLine{"100644", "f1", absent},
		treeLine{"100644", "f2", absent}, treeLine{"100644", "f3", absent}),
		16, "a", "b")

	w := newWalker(idx)
	err := w.walk("", root, 0)
	if !errors.Is(err, ErrUninspectable) || !strings.Contains(err.Error(), "paths") {
		t.Fatalf("walk = %v, want the maxChanges refusal", err)
	}
	if len(w.out) != maxChanges {
		t.Errorf("collected %d changes, want it to stop at %d", len(w.out), maxChanges)
	}
	if w.nodes > maxTreeNodes {
		t.Errorf("expanded %d trees: maxTreeNodes fired first, not maxChanges", w.nodes)
	}
}

// ─── hand-built hostile fixtures ────────────────────────────────────────────
//
// Only the fixtures a real git will never produce are assembled here.

type treeLine struct{ mode, name, oid string }

func mkTree(lines ...treeLine) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		raw, err := hex.DecodeString(l.oid)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, "%s %s", l.mode, l.name)
		b.WriteByte(0)
		b.Write(raw)
	}
	return b.Bytes()
}

func mkCommit(treeOID string) []byte {
	return fmt.Appendf(nil, "tree %s\nauthor T <t@example.com> 1767225600 +0000\n"+
		"committer T <t@example.com> 1767225600 +0000\n\nhostile\n", treeOID)
}

func hashObject(typ string, payload []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", typ, len(payload))
	h.Write([]byte{0})
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// deltaStream frames delta instructions with the two sizes a delta declares.
func deltaStream(baseSize, resultSize int64, instr ...[]byte) []byte {
	out := binary.AppendUvarint(nil, uint64(baseSize))
	out = binary.AppendUvarint(out, uint64(resultSize))
	for _, i := range instr {
		out = append(out, i...)
	}
	return out
}

func insertInstr(b []byte) []byte { return append([]byte{byte(len(b))}, b...) }

// copyInstr emits the always-4-byte-offset, always-2-byte-size form of a copy.
func copyInstr(offset, size int) []byte {
	return []byte{0x80 | 0x0f | 0x30,
		byte(offset), byte(offset >> 8), byte(offset >> 16), byte(offset >> 24),
		byte(size), byte(size >> 8)}
}

type rawObject struct {
	typ     objectType
	payload []byte
	backTo  int // OBJ_OFS_DELTA: bytes back from this object's start to its base's
}

func buildPack(t *testing.T, objs ...rawObject) []byte {
	t.Helper()
	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = binary.BigEndian.AppendUint32(pack, uint32(len(objs)))
	for _, o := range objs {
		pack = append(pack, objHeader(o.typ, int64(len(o.payload)))...)
		if o.typ == objOfsDelta {
			pack = append(pack, ofsEncoding(o.backTo)...)
		}
		pack = append(pack, deflate(t, o.payload)...)
	}
	sum := sha1.Sum(pack)
	return append(pack, sum[:]...)
}

func objHeader(typ objectType, size int64) []byte {
	b := []byte{byte(typ)<<4 | byte(size&0x0f)}
	for size >>= 4; size > 0; size >>= 7 {
		b[len(b)-1] |= 0x80
		b = append(b, byte(size&0x7f))
	}
	return b
}

// ofsEncoding is git's offset encoding: base-128, most significant byte first,
// with each continuation implicitly one larger.
func ofsEncoding(n int) []byte {
	var b []byte
	b = append(b, byte(n&0x7f))
	for n >>= 7; n > 0; n >>= 7 {
		n--
		b = append([]byte{byte(n&0x7f) | 0x80}, b...)
	}
	return b
}

func deflate(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("deflate: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("deflate close: %v", err)
	}
	return buf.Bytes()
}

func deflatedLen(t *testing.T, payload []byte) int { t.Helper(); return len(deflate(t, payload)) }

// commandSection frames one pkt-line command section, capabilities and all.
func commandSection(caps string, cmds ...string) []byte {
	var b bytes.Buffer
	for i, c := range cmds {
		line := c + "\n"
		if i == 0 {
			line = c + "\x00" + caps + "\n"
		}
		fmt.Fprintf(&b, "%04x%s", len(line)+4, line)
	}
	b.WriteString("0000")
	return b.Bytes()
}
