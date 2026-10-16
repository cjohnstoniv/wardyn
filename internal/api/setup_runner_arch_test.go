// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

func TestRunnerArchImagesCheck(t *testing.T) {
	images := map[string]string{"base": "m/base:1", "codex-cli": "m/codex:1"}
	variants := map[string]map[string]string{"arm64": {"codex-cli": "m/codex:1-arm64"}}

	chk, ok := runnerArchImagesCheck(map[string]int{"arm64": 3, "amd64": 5}, "amd64", images, variants)
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
		if _, ok := runnerArchImagesCheck(tc.counts, "amd64", tc.images, variants); ok {
			t.Errorf("%s: want no row", name)
		}
	}
	if chk, _ := runnerArchImagesCheck(map[string]int{"arm64": 1}, "amd64", images, nil); !strings.Contains(chk.Detail, "1 arm64 runner.") {
		t.Errorf("singular detail = %q", chk.Detail)
	}
}
