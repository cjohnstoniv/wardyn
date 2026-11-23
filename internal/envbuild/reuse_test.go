// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reuseBase = "ubuntu:24.04"

// reuseFixture is a builder over a fake daemon that holds reuseBase and a tools directory.
func reuseFixture(t *testing.T) (*fakeEnvbuilderDocker, *Builder) {
	t.Helper()
	f := newFakeEnvbuilderDocker()
	f.imagesPresent[reuseBase] = true
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)
	return f, b
}

func wrapReused(t *testing.T, b *Builder, scope string, reuse Reuse) (Built, bool) {
	t.Helper()
	reuse.Scope = scope
	built, reused, err := b.FinalizeBaseReused(context.Background(), reuseBase, reuse, nil)
	if err != nil {
		t.Fatalf("FinalizeBaseReused: %v", err)
	}
	return built, reused
}

func builds(f *fakeEnvbuilderDocker) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageBuilds
}

func TestFinalizeBaseReused_SecondWrapOfTheSameBaseIsReused(t *testing.T) {
	f, b := reuseFixture(t)
	first, reused := wrapReused(t, b, "sub-a", Reuse{})
	if reused || builds(f) != 1 {
		t.Fatalf("first wrap: reused=%v builds=%d, want a build", reused, builds(f))
	}
	second, reused := wrapReused(t, b, "sub-a", Reuse{})
	if !reused || builds(f) != 1 {
		t.Fatalf("second wrap: reused=%v builds=%d, want a reuse and no build", reused, builds(f))
	}
	hex64 := strings.TrimPrefix(second.Ref, "wardyn-byoi/reuse:")
	if hex64 == second.Ref || len(hex64) != 64 {
		t.Fatalf("ref %q is not wardyn-byoi/reuse:<64 hex>", second.Ref)
	}
	if second.Ref != first.Ref || second.ID != first.ID || second.BaseDigest != first.BaseDigest {
		t.Fatalf("reuse returned %+v, want %+v", second, first)
	}
}

func TestFinalizeBaseReused_KeyFollowsEveryInput(t *testing.T) {
	f, b := reuseFixture(t)
	base, _ := wrapReused(t, b, "sub-a", Reuse{})
	tool := filepath.Join(b.ToolsDir, requiredTools[0])
	cases := []struct {
		name         string
		change, undo func()
		scope        string
	}{
		{"base image id", func() { f.imageIDs[reuseBase] = "sha256:" + strings.Repeat("b", 64) }, func() { delete(f.imageIDs, reuseBase) }, "sub-a"},
		{"tool bytes", func() { _ = os.WriteFile(tool, []byte("#!/bin/sh\necho changed\n"), 0o755) }, func() { _ = os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755) }, "sub-a"},
		{"extra tool file", func() { _ = os.WriteFile(filepath.Join(b.ToolsDir, "extra"), []byte("x"), 0o755) }, func() { _ = os.Remove(filepath.Join(b.ToolsDir, "extra")) }, "sub-a"},
		{"scope", func() {}, func() {}, "sub-b"},
	}
	f.imageIDs = map[string]string{}
	for _, tc := range cases {
		before := builds(f)
		tc.change()
		got, reused := wrapReused(t, b, tc.scope, Reuse{})
		if reused || builds(f) != before+1 {
			t.Errorf("%s: reused=%v builds=%d, want one new build", tc.name, reused, builds(f)-before)
		}
		if got.Ref == base.Ref {
			t.Errorf("%s: tag %q equals the baseline tag", tc.name, got.Ref)
		}
		tc.undo()
		if again, _ := wrapReused(t, b, "sub-a", Reuse{}); again.Ref != base.Ref {
			t.Errorf("%s undone: tag %q, want the baseline tag %q", tc.name, again.Ref, base.Ref)
		}
	}
}

func TestFinalizeBaseReused_TagForAnotherKeyIsBuiltOver(t *testing.T) {
	f, b := reuseFixture(t)
	first, _ := wrapReused(t, b, "sub-a", Reuse{})
	key := strings.TrimPrefix(first.Ref, "wardyn-byoi/reuse:")
	for name, labels := range map[string]map[string]string{
		"no label":      nil,
		"another key":   {reuseKeyLabel: strings.Repeat("0", 64)},
		"unrelated key": {"other": key},
	} {
		f.mu.Lock()
		f.buildLabels[first.Ref] = labels
		before := f.imageBuilds
		f.mu.Unlock()
		got, reused := wrapReused(t, b, "sub-a", Reuse{})
		if reused || builds(f) != before+1 {
			t.Errorf("%s: reused=%v builds+%d, want a build", name, reused, builds(f)-before)
		}
		f.mu.Lock()
		label := f.buildLabels[got.Ref][reuseKeyLabel]
		f.mu.Unlock()
		if label != key {
			t.Errorf("%s: rebuilt label %q, want %q", name, label, key)
		}
	}
}

func TestFinalizeBaseReused_InspectFaultFailsTheBuild(t *testing.T) {
	f, b := reuseFixture(t)
	first, _ := wrapReused(t, b, "sub-a", Reuse{})
	f.mu.Lock()
	f.inspectErrs = map[string]error{first.Ref: errors.New("daemon unavailable")}
	f.mu.Unlock()
	_, _, err := b.FinalizeBaseReused(context.Background(), reuseBase, Reuse{Scope: "sub-a"}, nil)
	if err == nil || !strings.Contains(err.Error(), first.Ref) {
		t.Fatalf("err = %v, want one naming %q", err, first.Ref)
	}
	if builds(f) != 1 {
		t.Fatalf("builds = %d, want the one from the first call", builds(f))
	}
}

func TestFinalizeBaseReused_OnBuildBaseIsRefusedEvenWhenAWrapExists(t *testing.T) {
	f, b := reuseFixture(t)
	wrapReused(t, b, "sub-a", Reuse{})
	f.mu.Lock()
	f.onBuild = map[string][]string{reuseBase: {"RUN /bin/evil"}}
	f.mu.Unlock()
	_, _, err := b.FinalizeBaseReused(context.Background(), reuseBase, Reuse{Scope: "sub-a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ONBUILD") {
		t.Fatalf("err = %v, want an ONBUILD refusal", err)
	}
	if builds(f) != 1 {
		t.Fatalf("builds = %d, want only the first", builds(f))
	}
}

func TestFinalizeBaseReused_HoldsBeforeLookupAndAnnouncesOnlyABuild(t *testing.T) {
	f, b := reuseFixture(t)
	announced := 0
	var held []string
	reuse := Reuse{
		Hold: func(ref string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			held = append(held, ref)
			f.inspected = append(f.inspected, "HOLD:"+ref)
		},
		Announce: func() { announced++ },
	}
	first, _ := wrapReused(t, b, "sub-a", reuse)
	f.mu.Lock()
	for _, ref := range f.inspected {
		if ref == first.Ref {
			t.Errorf("the first call inspected the tag %q", ref)
		}
	}
	f.mu.Unlock()
	if announced != 1 {
		t.Fatalf("announced %d times after a build, want 1", announced)
	}
	wrapReused(t, b, "sub-a", reuse)
	if announced != 1 {
		t.Fatalf("announced %d times after a reuse, want still 1", announced)
	}
	if len(held) != 2 || held[1] != first.Ref {
		t.Fatalf("held %v, want the tag on each call", held)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	hold, look := -1, -1
	for i, ref := range f.inspected {
		switch {
		case ref == "HOLD:"+first.Ref:
			hold = i // the second call's hold is the last one
		case ref == first.Ref:
			look = i
		}
	}
	if look < 0 || hold < 0 || hold > look {
		t.Fatalf("inspect order %v: want the hold before the tag's inspect", f.inspected)
	}
}

func TestReusableBuild_EmptyScopeIsRefused(t *testing.T) {
	f, b := reuseFixture(t)
	if _, _, err := b.FinalizeBaseReused(context.Background(), reuseBase, Reuse{}, nil); err == nil {
		t.Fatal("an empty scope was accepted")
	}
	if len(f.inspected) != 0 || f.createCalled != 0 || f.imageBuilds != 0 {
		t.Fatalf("daemon calls made: inspected=%v creates=%d builds=%d", f.inspected, f.createCalled, f.imageBuilds)
	}
}

func TestFinalizeBase_FreshWrapKeepsItsTagAndSetsNoLabel(t *testing.T) {
	f, b := reuseFixture(t)
	const tag = "wardyn-byoi/run-1:latest"
	if _, err := b.FinalizeBase(context.Background(), reuseBase, tag, nil, false); err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if len(f.lastBuildTags) != 1 || f.lastBuildTags[0] != tag {
		t.Fatalf("tagged %v, want [%s]", f.lastBuildTags, tag)
	}
	if got := f.buildLabels[tag]; got != nil {
		t.Fatalf("labels %v on a fresh wrap, want none", got)
	}
}

func TestFinalizeBaseReused_ATagThisBuilderDidNotBuildIsBuiltOver(t *testing.T) {
	// Learn the tag the wrap will get from a throwaway daemon with the same inputs.
	f0, b0 := reuseFixture(t)
	probe, _ := wrapReused(t, b0, "sub-a", Reuse{})
	_ = f0
	key := strings.TrimPrefix(probe.Ref, "wardyn-byoi/reuse:")

	f := newFakeEnvbuilderDocker()
	f.imagesPresent[reuseBase] = true
	f.imagesPresent[probe.Ref] = true
	f.buildLabels = map[string]map[string]string{probe.Ref: {reuseKeyLabel: key}}
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = b0.ToolsDir

	if _, reused := wrapReused(t, b, "sub-a", Reuse{}); reused || builds(f) != 1 {
		t.Fatalf("planted tag: reused=%v builds=%d, want a build over it", reused, builds(f))
	}
	restarted := newWithClient(f, "envbuilder:test", "")
	restarted.ToolsDir = b0.ToolsDir
	if _, reused := wrapReused(t, restarted, "sub-a", Reuse{}); reused || builds(f) != 2 {
		t.Fatalf("after a restart: reused=%v builds=%d, want a build", reused, builds(f))
	}
	f.imageIDs = map[string]string{probe.Ref: "sha256:" + strings.Repeat("a", 64)}
	if _, reused := wrapReused(t, restarted, "sub-a", Reuse{}); reused || builds(f) != 3 {
		t.Fatalf("another image under the tag: reused=%v builds=%d, want a build", reused, builds(f))
	}
}
