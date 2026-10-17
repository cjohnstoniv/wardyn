// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunnerArchImagesCheck(t *testing.T) {
	images := map[string]string{"base": "m/base:1", "codex-cli": "m/codex:1"}
	variants := map[string]map[string]string{"arm64": {"codex-cli": "m/codex:1-arm64"}}

	chk, ok := runnerArchImagesCheck(map[string]int{"arm64": 3, "amd64": 5}, "amd64", images, variants, noDescribe)
	if !ok {
		t.Fatal("no row for arm64 runners with an uncovered key")
	}
	if chk.ID != "runner_arch_images" || chk.Status != "info" {
		t.Errorf("row = %+v; want an info runner_arch_images", chk)
	}
	assertSetupCheckBlocking(t, chk)
	if !strings.Contains(chk.Detail, "3 arm64 runners") || !strings.Contains(chk.Detail, "no arm64 image: base.") {
		t.Errorf("detail %q lists the wrong keys (codex-cli is covered)", chk.Detail)
	}
	if !strings.Contains(chk.Fix, `"base@arm64"`) {
		t.Errorf("fix %q does not name the first uncovered key", chk.Fix)
	}
	for _, want := range []string{"docker buildx build --platform linux/amd64,linux/arm64", "docker buildx imagetools inspect"} {
		if !strings.Contains(chk.Fix, want) {
			t.Errorf("fix %q lacks %q", chk.Fix, want)
		}
	}

	for name, tc := range map[string]struct {
		counts map[string]int
		images map[string]string
	}{
		"no foreign-arch runner": {map[string]int{"amd64": 4}, images},
		"no runners":             {nil, images},
		"every key covered":      {map[string]int{"arm64": 1}, map[string]string{"codex-cli": "x"}},
		"no operator keys":       {map[string]int{"arm64": 1}, nil},
	} {
		if _, ok := runnerArchImagesCheck(tc.counts, "amd64", tc.images, variants, noDescribe); ok {
			t.Errorf("%s: want no row", name)
		}
	}
	if chk, _ := runnerArchImagesCheck(map[string]int{"arm64": 1}, "amd64", images, nil, noDescribe); !strings.Contains(chk.Detail, "1 arm64 runner.") {
		t.Errorf("singular detail = %q", chk.Detail)
	}
}

func noDescribe(string) string { return "" }

// fakeDescriber answers DescribeImage from canned text; a ref without text is absent from the store.
type fakeDescriber map[string]string

func (f fakeDescriber) DescribeImage(_ context.Context, ref string) (string, error) {
	if d, ok := f[ref]; ok {
		return d, nil
	}
	return "", errors.New("no such image")
}

func TestRunnerArchImagesCheckNamesTheSelectedImage(t *testing.T) {
	const (
		single = "m/single:1"
		index  = "m/index:1"
		absent = "m/absent:1"
	)
	f := fakeDescriber{
		single: "m/single@sha256:aa, a single-platform manifest; platform linux/amd64",
		index:  "m/index@sha256:bb, a multi-arch index; platform linux/amd64",
	}
	images := map[string]string{"s": single, "i": index, "u": absent}
	chk, ok := runnerArchImagesCheck(map[string]int{"arm64": 2}, "amd64", images, nil, func(ref string) string {
		return describeLocalImage(context.Background(), f.DescribeImage, ref)
	})
	if !ok {
		t.Fatal("no row")
	}
	for _, want := range []string{
		"s = m/single:1: m/single@sha256:aa, a single-platform manifest; platform linux/amd64",
		"i = m/index:1: m/index@sha256:bb, a multi-arch index; platform linux/amd64",
		"u = m/absent:1: cannot tell from wardynd's local image store",
		"docker buildx imagetools inspect m/absent:1",
		"may have copied one platform only, or a platform pin may be at fault; neither is known from here",
	} {
		if !strings.Contains(chk.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, chk.Detail)
		}
	}
	// No store at all (a runner that is not docker) reads as unknown too.
	if got := describeLocalImage(context.Background(), nil, single); !strings.Contains(got, "cannot tell") {
		t.Errorf("nil describer = %q", got)
	}
}

// When every selected image is a multi-arch index the mirror-copy suspicion has nothing to stand on,
// so the sentence is left out; one image that is not an index keeps it.
func TestRunnerArchImagesCheckOmitsTheMirrorSentenceForIndexes(t *testing.T) {
	const blame = "may have copied one platform only"
	desc := map[string]string{
		"m/a:1": "m/a@sha256:aa, a multi-arch index; platform linux/amd64",
		"m/b:1": "m/b@sha256:bb, a multi-arch index; platform linux/amd64",
		"m/c:1": "m/c@sha256:cc, a single-platform manifest; platform linux/amd64",
	}
	detail := func(images map[string]string) string {
		chk, ok := runnerArchImagesCheck(map[string]int{"arm64": 1}, "amd64", images, nil, func(ref string) string { return desc[ref] })
		if !ok {
			t.Fatal("no row")
		}
		return chk.Detail
	}
	if d := detail(map[string]string{"a": "m/a:1", "b": "m/b:1"}); strings.Contains(d, blame) {
		t.Errorf("an all-index row blames a mirror: %s", d)
	}
	if d := detail(map[string]string{"a": "m/a:1", "c": "m/c:1"}); !strings.Contains(d, blame) {
		t.Errorf("a row with a single-platform image dropped the mirror sentence: %s", d)
	}
}
