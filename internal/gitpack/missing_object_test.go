// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	missingOIDA = "1111111111111111111111111111111111111111"
	missingOIDB = "2222222222222222222222222222222222222222"
)

// carriedCommit is a self-contained one-commit pack (a commit and its empty
// tree) and the commit's id.
func carriedCommit(t *testing.T, extra ...rawObject) (pack []byte, commitOID string) {
	t.Helper()
	commit := mkCommit(hashObject("tree", nil))
	objs := append([]rawObject{{typ: objCommit, payload: commit}, {typ: objTree, payload: nil}}, extra...)
	return buildPack(t, objs...), hashObject("commit", commit)
}

func pushOf(pack []byte, cmds ...string) []byte {
	return append(commandSection("report-status object-format=sha1", cmds...), pack...)
}

func cmdFor(newOID, ref string) string { return strings.Repeat("0", 40) + " " + newOID + " " + ref }

func missingRefs(t *testing.T, err error) []string {
	t.Helper()
	var m *MissingObjectError
	if !errors.As(err, &m) {
		t.Fatalf("err = %v, want a *MissingObjectError", err)
	}
	if !errors.Is(err, ErrUninspectable) {
		t.Errorf("err = %v, want it to stay an ErrUninspectable", err)
	}
	return m.Refs
}

// A branch created at a commit the receiving side already has sends an empty
// pack; the ref is named as the one whose target is missing.
func TestPackInspect_MissingTargetNamesTheRef(t *testing.T) {
	_, err := Inspect(pushOf(buildPack(t), cmdFor(missingOIDA, "refs/heads/second")))
	if got := missingRefs(t, err); !slices.Equal(got, []string{"refs/heads/second"}) {
		t.Errorf("Refs = %v, want [refs/heads/second]", got)
	}
}

// One ref's commit is in the pack and another's is not: only the second is named.
func TestPackInspect_MixedPushNamesOnlyTheMissingRef(t *testing.T) {
	pack, commit := carriedCommit(t)
	_, err := Inspect(pushOf(pack,
		cmdFor(commit, "refs/heads/carried"),
		cmdFor(missingOIDA, "refs/heads/absent"),
		cmdFor(missingOIDB, "refs/heads/absent2")))
	if got := missingRefs(t, err); !slices.Equal(got, []string{"refs/heads/absent", "refs/heads/absent2"}) {
		t.Errorf("Refs = %v, want only the two absent refs", got)
	}
	if _, err := Inspect(pushOf(pack, cmdFor(commit, "refs/heads/carried"))); err != nil {
		t.Errorf("the carried ref alone: %v, want it inspected", err)
	}
}

// No pack at all, with an update: every updated ref is named, a delete is not.
func TestPackInspect_NoPackNamesEveryUpdatedRef(t *testing.T) {
	body := commandSection("report-status object-format=sha1",
		cmdFor(missingOIDA, "refs/heads/a"),
		missingOIDB+" "+strings.Repeat("0", 40)+" refs/heads/gone",
		cmdFor(missingOIDB, "refs/heads/b"))
	_, err := Inspect(body)
	if got := missingRefs(t, err); !slices.Equal(got, []string{"refs/heads/a", "refs/heads/b"}) {
		t.Errorf("Refs = %v, want the two updates and not the delete", got)
	}
	if _, err := Inspect(commandSection("report-status object-format=sha1",
		missingOIDB+" "+strings.Repeat("0", 40)+" refs/heads/gone")); err != nil {
		t.Errorf("delete-only: %v, want no refusal", err)
	}
}

// A tag whose peeled target is absent is a missing object too.
func TestPackInspect_TagToAbsentCommitIsMissing(t *testing.T) {
	tag := fmt.Appendf(nil, "object %s\ntype commit\ntag v1\ntagger T <t@example.com> 1767225600 +0000\n\nx\n", missingOIDA)
	pack := buildPack(t, rawObject{typ: objTag, payload: tag})
	_, err := Inspect(pushOf(pack, cmdFor(hashObject("tag", tag), "refs/tags/v1")))
	if got := missingRefs(t, err); !slices.Equal(got, []string{"refs/tags/v1"}) {
		t.Errorf("Refs = %v, want [refs/tags/v1]", got)
	}
}

// A target that is present but unreadable as a commit is not "absent": it keeps
// the existing reason, so the text does not claim the request lacks it.
func TestPackInspect_UnreadableTargetKeepsItsReason(t *testing.T) {
	blob := []byte("not a commit\n")
	badTag := []byte("tagger T <t@example.com> 1767225600 +0000\n\nno object header\n")
	_, commit := carriedCommit(t)
	chain := [][]byte{fmt.Appendf(nil, "object %s\ntype commit\ntag t8\n\nx\n", commit)}
	for k := 7; k >= 0; k-- { // nine tags: past maxPeel
		chain = append(chain, fmt.Appendf(nil, "object %s\ntype tag\ntag t%d\n\nx\n", hashObject("tag", chain[len(chain)-1]), k))
	}
	for _, tc := range []struct {
		name string
		objs []rawObject
		tip  string
	}{
		{"wrong type", []rawObject{{typ: objBlob, payload: blob}}, hashObject("blob", blob)},
		{"malformed tag", []rawObject{{typ: objTag, payload: badTag}}, hashObject("tag", badTag)},
		{"peel limit", nil, hashObject("tag", chain[len(chain)-1])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := tc.objs
			if tc.name == "peel limit" {
				for _, c := range chain {
					objs = append(objs, rawObject{typ: objTag, payload: c})
				}
			}
			pack, _ := carriedCommit(t, objs...)
			_, err := Inspect(pushOf(pack, cmdFor(tc.tip, "refs/heads/x")))
			if !errors.Is(err, ErrUninspectable) {
				t.Fatalf("err = %v, want ErrUninspectable", err)
			}
			var m *MissingObjectError
			if errors.As(err, &m) {
				t.Errorf("err = %v, an object the pack holds must not be reported missing", err)
			}
			if !strings.Contains(err.Error(), "does not carry as a commit") {
				t.Errorf("err = %v, want the existing reason", err)
			}
		})
	}
}

// Commands is set exactly when the request carries no object: an empty pack
// or none at all. A pack carrying anything (another ref's commit, a tag that
// peels to an absent commit) adds content, so it leaves Commands unset.
func TestPackInspect_MissingCommandsOnlyWhenNothingIsCarried(t *testing.T) {
	carried, commit := carriedCommit(t)
	tag := fmt.Appendf(nil, "object %s\ntype commit\ntag v1\ntagger T <t@example.com> 1767225600 +0000\n\nx\n", missingOIDA)
	for _, tc := range []struct {
		name string
		body []byte
		want int
	}{
		{"empty pack", pushOf(buildPack(t), cmdFor(missingOIDA, "refs/heads/a"), cmdFor(missingOIDB, "refs/heads/b")), 2},
		{"no pack", commandSection("report-status object-format=sha1", cmdFor(missingOIDA, "refs/heads/a")), 1},
		{"a pack carrying another ref", pushOf(carried, cmdFor(commit, "refs/heads/c"), cmdFor(missingOIDA, "refs/heads/a")), 0},
		{"a pack carrying the tag", pushOf(buildPack(t, rawObject{typ: objTag, payload: tag}), cmdFor(hashObject("tag", tag), "refs/tags/v1")), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Inspect(tc.body)
			var m *MissingObjectError
			if !errors.As(err, &m) {
				t.Fatalf("err = %v, want a *MissingObjectError", err)
			}
			if len(m.Commands) != tc.want {
				t.Errorf("Commands = %v, want %d", m.Commands, tc.want)
			}
		})
	}
}
