// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func hex64Of(c byte) string { return strings.Repeat(string(c), 64) }

func TestListReuseImages_ListsOnlyReuseTagsCarryingTheirKey(t *testing.T) {
	k1, k2, k3, k4 := hex64Of('1'), hex64Of('2'), hex64Of('3'), hex64Of('4')
	tag := func(k string) string { return byoiReuseRepo + ":" + k }
	f := newFakeEnvbuilderDocker()
	for _, ref := range []string{tag(k1), tag(k2), byoiReuseRepo + ":abc", tag(k4), "wardyn-byoi/0f1e2d3c-0000-4000-8000-000000000000:latest"} {
		f.imagesPresent[ref] = true
	}
	f.buildLabels = map[string]map[string]string{
		tag(k1):                {reuseKeyLabel: k1},
		tag(k2):                {reuseKeyLabel: k3},
		byoiReuseRepo + ":abc": {reuseKeyLabel: "abc"},
		"wardyn-byoi/0f1e2d3c-0000-4000-8000-000000000000:latest": {reuseKeyLabel: k1},
	}
	f.created = map[string]int64{tag(k1): 1000}
	b := newWithClient(f, "envbuilder:test", "")

	got, err := b.ListReuseImages(t.Context())
	if err != nil {
		t.Fatalf("ListReuseImages: %v", err)
	}
	want := []ReuseImage{{Ref: tag(k1), Created: time.Unix(1000, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListReuseImages = %+v, want %+v", got, want)
	}
}

func TestListReuseImages_ListsWhatAReusableBuildLeft(t *testing.T) {
	_, b := reuseFixture(t)
	built, _ := wrapReused(t, b, "sub-a", Reuse{})
	got, err := b.ListReuseImages(t.Context())
	if err != nil {
		t.Fatalf("ListReuseImages: %v", err)
	}
	if len(got) != 1 || got[0].Ref != built.Ref {
		t.Fatalf("ListReuseImages = %+v, want one entry for %s", got, built.Ref)
	}
}

func TestRemoveReuseImage_RemovesOnlyAReuseTagAndNeverForces(t *testing.T) {
	reuseRef := byoiReuseRepo + ":" + hex64Of('1')
	others := []string{"wardyn-byoi/0f1e2d3c-0000-4000-8000-000000000000:latest", "wardyn-workspace/w:built", "ghcr.io/x/y:1"}
	f := newFakeEnvbuilderDocker()
	f.imagesPresent[reuseRef] = true
	for _, ref := range others {
		f.imagesPresent[ref] = true
	}
	b := newWithClient(f, "envbuilder:test", "")

	if err := b.RemoveReuseImage(t.Context(), reuseRef); err != nil {
		t.Fatalf("RemoveReuseImage of a present tag: %v", err)
	}
	if !f.removedImage(reuseRef) || f.imagesPresent[reuseRef] {
		t.Fatalf("the reuse tag was not removed (removed=%v present=%v)", f.removedImage(reuseRef), f.imagesPresent[reuseRef])
	}
	if err := b.RemoveReuseImage(t.Context(), reuseRef); err != nil {
		t.Fatalf("RemoveReuseImage of a tag already gone: %v", err)
	}
	for _, ref := range others {
		if err := b.RemoveReuseImage(t.Context(), ref); err == nil {
			t.Errorf("RemoveReuseImage(%q) = nil, want a refusal", ref)
		}
		if !f.imagesPresent[ref] {
			t.Errorf("%q was removed", ref)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []string{reuseRef, reuseRef}; !reflect.DeepEqual(f.removedImages, want) {
		t.Fatalf("daemon saw removals %v, want only %v", f.removedImages, want)
	}
	if f.removeForced || !f.removePruned {
		t.Fatalf("removeForced=%v removePruned=%v, want false and true", f.removeForced, f.removePruned)
	}
}
