// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"fmt"
	"strings"
	"testing"
)

// bases renders a change set as one "path mode base" line per change.
func bases(changes []Change) string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, fmt.Sprintf("%s %s %q", c.Path, c.Mode, c.BaseMode))
	}
	return strings.Join(out, "\n")
}

// TestPackTree_BaseModeIsSetOnlyWhenTheParentTreeIsCarried: a diff against a
// carried parent knows what each path held before, so an added path, a mode
// change and an edit are told apart; a whole-tree walk, and a diff whose old
// tree the pack does not carry, say nothing about the base.
func TestPackTree_BaseModeIsSetOnlyWhenTheParentTreeIsCarried(t *testing.T) {
	blobA, blobK, blobK2, blobN := []byte("a\n"), []byte("k\n"), []byte("k two\n"), []byte("n\n")
	a, k, k2, n := hashObject("blob", blobA), hashObject("blob", blobK), hashObject("blob", blobK2), hashObject("blob", blobN)
	idx := newIndex(sha1Format)
	for _, b := range [][]byte{blobA, blobK, blobK2, blobN} {
		idx.put(objBlob, b)
	}
	oldSub := idx.put(objTree, mkTree(treeLine{"100644", "x.txt", a}))
	newSub := idx.put(objTree, mkTree(treeLine{"100644", "x.txt", a}, treeLine{"100755", "y.sh", n}))
	added := idx.put(objTree, mkTree(treeLine{"100755", "z.sh", n}))
	oldRoot := idx.put(objTree, mkTree(
		treeLine{"100644", "a.sh", a},
		treeLine{"100755", "keep.sh", k},
		treeLine{"40000", "sub", oldSub}))
	newRoot := idx.put(objTree, mkTree(
		treeLine{"100755", "a.sh", a},
		treeLine{"100755", "keep.sh", k2},
		treeLine{"100755", "new.sh", n},
		treeLine{"40000", "newdir", added},
		treeLine{"100775", "odd.sh", n},
		treeLine{"40000", "sub", newSub}))

	w := newWalker(idx)
	if err := w.diff("", oldRoot, newRoot, 0); err != nil {
		t.Fatalf("diff: %v", err)
	}
	want := strings.Join([]string{
		`a.sh 100755 "100644"`,
		`keep.sh 100755 "100755"`,
		`new.sh 100755 "0"`,
		`newdir/z.sh 100755 "0"`,
		`odd.sh 100775 "0"`,
		`sub/y.sh 100755 "0"`,
	}, "\n")
	if got := bases(w.out); got != want {
		t.Errorf("diffed:\n%s\nwant:\n%s", got, want)
	}

	w = newWalker(idx)
	if err := w.walk("", newRoot, 0, ""); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, c := range w.out {
		if c.BaseMode != "" {
			t.Errorf("walked %s has BaseMode %q, want none: no carried parent says", c.Path, c.BaseMode)
		}
	}

	w = newWalker(idx)
	if err := w.diff("", strings.Repeat("ab", 20), newRoot, 0); err != nil {
		t.Fatalf("diff against an uncarried tree: %v", err)
	}
	for _, c := range w.out {
		if c.BaseMode != "" {
			t.Errorf("%s diffed against an uncarried tree has BaseMode %q, want none", c.Path, c.BaseMode)
		}
	}
}

// TestPackTree_AnEntryTwoParentsReportIsOneChangeWithTheNewestBase: the base is
// not part of an entry's identity, so the change set has exactly the entries it
// had before bases existed, and an entry new against either parent stays new
// whichever parent is compared first.
func TestPackTree_AnEntryTwoParentsReportIsOneChangeWithTheNewestBase(t *testing.T) {
	blobOld, blobNew := []byte("old\n"), []byte("new\n")
	old, nw := hashObject("blob", blobOld), hashObject("blob", blobNew)
	for _, order := range [][2]string{{"executable first", "absent first"}, {"absent first", "executable first"}} {
		t.Run(order[0], func(t *testing.T) {
			idx := newIndex(sha1Format)
			idx.put(objBlob, blobOld)
			idx.put(objBlob, blobNew)
			roots := map[string]string{
				"executable first": idx.put(objTree, mkTree(treeLine{"100755", "t.sh", old})),
				"absent first":     idx.put(objTree, mkTree(treeLine{"100644", "u.txt", old})),
			}
			newRoot := idx.put(objTree, mkTree(treeLine{"100755", "t.sh", nw}))
			w := newWalker(idx)
			for _, o := range order {
				if err := w.diff("", roots[o], newRoot, 0); err != nil {
					t.Fatalf("diff: %v", err)
				}
			}
			if got := bases(w.out); got != `t.sh 100755 "0"` {
				t.Errorf("changes = %s, want one t.sh new against the parent that lacked it", got)
			}
		})
	}
}

// TestPackChange_IsExecutable follows git's checkout: a regular file with the
// user-execute bit is checked out 0755 whatever else its mode spells.
func TestPackChange_IsExecutable(t *testing.T) {
	for mode, want := range map[string]bool{
		"100755": true, "100775": true, "100744": true, "100700": true, "100711": true,
		"100644": false, "100664": false, "100600": false, "120000": false, "160000": false,
		"040000": false, "40000": false, "40755": false, ModeAbsent: false, "": false, "100x55": false,
	} {
		if got := IsExecutable(mode); got != want {
			t.Errorf("IsExecutable(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestPackTree_NewerBase(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "", ""},
		{"100755", "100755", "100755"},
		{"100755", "", ""},
		{"", "100755", ""},
		{"100755", ModeAbsent, ModeAbsent},
		{ModeAbsent, "100755", ModeAbsent},
		{"", "100644", "100644"},
		{"100775", ModeAbsent, ModeAbsent},
		{"100775", "", ""},
		{"100644", "", "100644"},
	}
	for _, c := range cases {
		if got := newerBase(c.a, c.b); got != c.want {
			t.Errorf("newerBase(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// TestPackInspect_BaseModeOnARealPush: the first commit of a push builds on a
// parent the forge holds, so its tree is walked with no base; the second is
// diffed against the first, which the pack carries.
func TestPackInspect_BaseModeOnARealPush(t *testing.T) {
	r := newRepo(t)
	r.write("run.sh", "#!/bin/sh\n", 0o755)
	r.write("notes.txt", "notes\n", 0o644)
	r.commit("seed")
	r.push("HEAD:refs/heads/main", "--no-thin")

	r.write("README.md", "hello\n", 0o644)
	r.commit("one")
	r.write("new.sh", "#!/bin/sh\necho new\n", 0o755)
	r.git("update-index", "--chmod=+x", "notes.txt")
	r.write("run.sh", "#!/bin/sh\necho edited\n", 0o755)
	r.commit("two")
	res, err := Inspect(r.push("HEAD:refs/heads/wardyn/run/work", "--no-thin"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	want := strings.Join([]string{
		`README.md 100644 ""`,
		`new.sh 100755 "0"`,
		`notes.txt 100644 ""`,
		`notes.txt 100755 "100644"`,
		`run.sh 100755 ""`,
		`run.sh 100755 "100755"`,
	}, "\n")
	if got := bases(res.Changes); got != want {
		t.Errorf("changes:\n%s\nwant:\n%s", got, want)
	}
}
