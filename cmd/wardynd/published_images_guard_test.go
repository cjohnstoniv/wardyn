// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The set of images we PUBLISH is asserted in exactly one place — release.yml's
// publish matrix — and consumed by hand in several others. Those hand-copies
// drifted, in both directions, and each drift was quiet:
//
//   - scripts/gpl-source-offer.sh listed agent-claude-code long after 0.6.2
//     stopped publishing it, and omitted agent-base which publishes in its
//     place. So the loop errored on a ref that does not exist (it is an
//     interactive paste with no `set -e`, so it carried on) while the image that
//     IS published was never scanned — the corresponding-source offer for it
//     simply did not exist.
//   - RELEASING.md's manual multi-arch cosign verification had the same list,
//     with the same two errors.
//
// Both are legal/supply-chain surfaces, and both fail SILENTLY: a missing image
// produces no output rather than an error. This is the check that makes the
// drift loud.
func TestPublishedImageListsAgree(t *testing.T) {
	root := repoRoot(t)

	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	// The matrix entries are `- name: <image>` lines inside the images job.
	// Parsed by regex rather than YAML so this guard needs no vendored parser.
	nameRe := regexp.MustCompile(`(?m)^\s+- name: ([a-z][a-z0-9-]*)\s*$`)
	published := map[string]bool{}
	for _, m := range nameRe.FindAllStringSubmatch(string(wf), -1) {
		published[m[1]] = true
	}
	if len(published) < 3 {
		t.Fatalf("parsed only %d published images from release.yml (%v) — the parse regressed and this guard would pass vacuously", len(published), keys(published))
	}
	if !published["agent-base"] {
		t.Errorf("release.yml no longer publishes agent-base; if that is deliberate, this guard and the consumers below need re-deriving")
	}

	// scripts/gpl-source-offer.sh's IMAGES=(...) array.
	sh, err := os.ReadFile(filepath.Join(root, "scripts", "gpl-source-offer.sh"))
	if err != nil {
		t.Fatalf("read gpl-source-offer.sh: %v", err)
	}
	arrRe := regexp.MustCompile(`(?m)^IMAGES=\(([^)]*)\)`)
	m := arrRe.FindStringSubmatch(string(sh))
	if m == nil {
		t.Fatal("could not find IMAGES=(...) in scripts/gpl-source-offer.sh — this guard can no longer see the list it exists to check")
	}
	offer := map[string]bool{}
	for _, f := range strings.Fields(m[1]) {
		offer[f] = true
	}

	var missing, extra []string
	for img := range published {
		if !offer[img] {
			missing = append(missing, img)
		}
	}
	for img := range offer {
		if !published[img] {
			extra = append(extra, img)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("published images with NO corresponding-source offer: %v\n"+
			"Publishing an image conveys its GPL/LGPL binaries; an image absent from this list has no offer at all.", missing)
	}
	if len(extra) > 0 {
		t.Errorf("gpl-source-offer.sh scans images we do NOT publish: %v\n"+
			"The scan silently produces nothing for them, which reads as 'no GPL packages' rather than 'never scanned'.", extra)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
