// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestPackTips_WhatEachRefsTreeHoldsWhereAsked: Changes says what each commit
// introduces, which a merge or a second ref can satisfy from an older commit.
// Tips says what each ref's new tree holds at the paths asked about: a
// directory the pack carries is walked into, one it does not is one entry the
// receiving side's contents stand for, and a walk past its budget is an error,
// never a short answer.
func TestPackTips_WhatEachRefsTreeHoldsWhereAsked(t *testing.T) {
	r := newRepo(t)
	r.write(".github/workflows/ci.yml", "on: push\n", 0o644)
	r.write("lib/a.txt", "a\n", 0o644)
	a := r.commit("a")
	r.write(".github/workflows/ci.yml", "on: reviewed\n", 0o644)
	r.commit("b")
	r.push("HEAD:refs/heads/main")
	r.write(".github/workflows/ci.yml", "on: changed\n", 0o644)
	c := r.commit("c")
	r.git("checkout", "-q", "--detach", a)
	r.write("lib/d.txt", "d\n", 0o644)
	d := r.commit("d")
	r.git("push", "-q", "--receive-pack="+r.script, r.remote, c+":refs/heads/c", d+":refs/heads/d")
	body, err := os.ReadFile(r.bodyAt)
	if err != nil {
		t.Fatalf("read captured body: %v", err)
	}
	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	asked := func(c Change) (bool, error) {
		return c.Path == ".github" || strings.HasPrefix(c.Path, ".github/"), nil
	}
	tips, err := res.Tips(asked)
	if err != nil {
		t.Fatalf("Tips: %v", err)
	}
	want := map[string][]string{
		"refs/heads/c": {fmt.Sprintf(".github/workflows/ci.yml 100644 %s carried", r.git("rev-parse", c+":.github/workflows/ci.yml"))},
		"refs/heads/d": {fmt.Sprintf(".github 40000 %s uncarried", r.git("rev-parse", a+":.github"))},
	}
	if len(tips) != len(res.Commands) {
		t.Fatalf("Tips = %d lists, want one per command (%d)", len(tips), len(res.Commands))
	}
	for i, cmd := range res.Commands {
		var got []string
		for _, e := range tips[i] {
			how := map[bool]string{true: "carried", false: "uncarried"}[e.Carried()]
			got = append(got, fmt.Sprintf("%s %s %s %s", e.Path, e.Mode, e.OID, how))
		}
		if !slices.Equal(got, want[cmd.Ref]) {
			t.Errorf("%s holds %v, want %v", cmd.Ref, got, want[cmd.Ref])
		}
	}
	prev := maxTipNodes
	t.Cleanup(func() { maxTipNodes = prev })
	maxTipNodes = 1
	if _, err := res.Tips(asked); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Tips past maxTipNodes = %v, want an ErrTooLarge", err)
	}
}
