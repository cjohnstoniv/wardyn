// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		t.Fatalf("parsed only %d published images from release.yml (%v) — the parse regressed and this guard would pass vacuously", len(published), slices.Sorted(maps.Keys(published)))
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

	// RELEASING.md's manual multi-arch cosign verification: the SECOND
	// hand-copy this guard's doc comment names, and the one it did not read.
	// Its `for img in …; do` loop is parsed the same regex-not-YAML way, and it
	// drifts the same way — silently, because `docker buildx imagetools
	// inspect` on a ref that does not exist prints an error the maintainer
	// pasting the block has no `set -e` to stop on, while the image that IS
	// published is simply never verified.
	doc, err := os.ReadFile(filepath.Join(root, "RELEASING.md"))
	if err != nil {
		t.Fatalf("read RELEASING.md: %v", err)
	}
	loopRe := regexp.MustCompile(`(?m)^\s*for img in ([a-z0-9 -]+); do\s*$`)
	lm := loopRe.FindStringSubmatch(string(doc))
	if lm == nil {
		t.Fatal("could not find RELEASING.md's `for img in …; do` cosign-verification loop — this guard can no longer see the list it exists to check")
	}
	verify := map[string]bool{}
	for _, f := range strings.Fields(lm[1]) {
		verify[f] = true
	}
	if len(verify) < 3 {
		t.Fatalf("parsed only %d images from RELEASING.md's verification loop (%v) — the parse regressed and this arm would pass vacuously", len(verify), slices.Sorted(maps.Keys(verify)))
	}

	var missing, extra, unverified, phantom []string
	for img := range published {
		if !offer[img] {
			missing = append(missing, img)
		}
		if !verify[img] {
			unverified = append(unverified, img)
		}
	}
	for img := range offer {
		if !published[img] {
			extra = append(extra, img)
		}
	}
	for img := range verify {
		if !published[img] {
			phantom = append(phantom, img)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(unverified)
	sort.Strings(phantom)

	if len(missing) > 0 {
		t.Errorf("published images with NO corresponding-source offer: %v\n"+
			"Publishing an image conveys its GPL/LGPL binaries; an image absent from this list has no offer at all.", missing)
	}
	if len(extra) > 0 {
		t.Errorf("gpl-source-offer.sh scans images we do NOT publish: %v\n"+
			"The scan silently produces nothing for them, which reads as 'no GPL packages' rather than 'never scanned'.", extra)
	}
	if len(unverified) > 0 {
		t.Errorf("published images RELEASING.md's release verification never checks: %v\n"+
			"The maintainer runs that loop to confirm each published image is a multi-arch index with a cosign signature on it; an image absent from the loop ships unverified.", unverified)
	}
	if len(phantom) > 0 {
		t.Errorf("RELEASING.md verifies images we do NOT publish: %v\n"+
			"`docker buildx imagetools inspect` on a ref that does not exist just errors, and the paste has no `set -e` — so the loop carries on and the maintainer reads it as verified.", phantom)
	}
}
