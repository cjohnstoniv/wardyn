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
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
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
// an ordinary second push, INCLUDING both documented ceilings: dir/unchanged.txt
// is over-reported because its parent directory changed and the pre-image tree is
// not in the pack, and keep/u.txt is absent because its directory is byte for
// byte one the receiving side already stores.
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
	w := &walker{idx: idx, seen: map[Change]bool{}}
	if err := w.diff("", hashObject("tree", oldRoot), hashObject("tree", newRoot), 0); err != nil {
		t.Fatalf("diff: %v", err)
	}
	wantChanges(t, w.out, "dir/y.txt 100644 6")
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
