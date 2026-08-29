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

// The demo video manifest (ui/src/app/lib/demo-videos.ts) and README.md each
// name the same 13 shipped episodes' (tag, file) pairs independently — the
// manifest so the console can build a download URL, README so a browser on
// GitHub can. They drift the moment one is edited and not the other: a
// re-shoot's "one sed" (RELEASING.md step 7) is supposed to rewrite both, and
// this is the check that catches the day it doesn't.
//
// Every SHIPPED manifest entry carries its tag as a literal "v…" string, not
// a shared TS constant, precisely so this Go-side regex can see it without
// evaluating TypeScript.
func TestDemoVideoManifestMatchesREADME(t *testing.T) {
	root := repoRoot(t)

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	linkRe := regexp.MustCompile(`releases/download/([^/]+)/(wardyn-[a-z0-9-]+\.mp4)`)
	readmePairs := map[[2]string]bool{}
	for _, m := range linkRe.FindAllStringSubmatch(string(readme), -1) {
		readmePairs[[2]string{m[1], m[2]}] = true
	}
	if len(readmePairs) < 13 {
		t.Fatalf("parsed only %d (tag, file) pairs from README.md — the parse regressed and this guard would pass vacuously", len(readmePairs))
	}

	manifest, err := os.ReadFile(filepath.Join(root, "ui", "src", "app", "lib", "demo-videos.ts"))
	if err != nil {
		t.Fatalf("read ui/src/app/lib/demo-videos.ts: %v", err)
	}
	// Only the EPISODES array's own object literals match: each starts with
	// `{ id: "<quoted>"`. The Episode interface's one-line declaration also
	// reads `{ id: string; ... }`, but with no quote after `id:`, so it never
	// matches — no need to slice the file to exclude it.
	entryRe := regexp.MustCompile(`\{\s*id:\s*"[^"]+"[^}]*\}`)
	entries := entryRe.FindAllString(string(manifest), -1)
	if len(entries) < 21 {
		t.Fatalf("parsed only %d demo-videos.ts entries (13 shipped + 8 reserved expected) — the parse regressed and this guard would pass vacuously", len(entries))
	}

	tagFieldRe := regexp.MustCompile(`tag:\s*(null|"v[^"]*")`)
	fileFieldRe := regexp.MustCompile(`file:\s*"([^"]+)"`)
	fileNameRe := regexp.MustCompile(`^wardyn-\d[0-9a-z]*-[a-z0-9-]+\.mp4$`)

	manifestPairs := map[[2]string]bool{}
	nullCount := 0
	for _, entry := range entries {
		tagM := tagFieldRe.FindStringSubmatch(entry)
		if tagM == nil {
			t.Fatalf("manifest entry's tag: is neither a \"v…\" string literal nor null: %s", entry)
		}
		fileM := fileFieldRe.FindStringSubmatch(entry)
		if fileM == nil {
			t.Fatalf("manifest entry has no file: field: %s", entry)
		}
		file := fileM[1]
		if !fileNameRe.MatchString(file) {
			t.Errorf("manifest file %q does not match ^wardyn-\\d[0-9a-z]*-[a-z0-9-]+\\.mp4$", file)
		}
		if tagM[1] == "null" {
			nullCount++
			continue
		}
		manifestPairs[[2]string{strings.Trim(tagM[1], `"`), file}] = true
	}
	if nullCount < 1 {
		t.Fatalf("parsed 0 reserved (tag: null) manifest entries — expected at least one; the parse regressed")
	}

	var missingFromManifest, missingFromREADME []string
	for p := range readmePairs {
		if !manifestPairs[p] {
			missingFromManifest = append(missingFromManifest, p[0]+"/"+p[1])
		}
	}
	for p := range manifestPairs {
		if !readmePairs[p] {
			missingFromREADME = append(missingFromREADME, p[0]+"/"+p[1])
		}
	}
	sort.Strings(missingFromManifest)
	sort.Strings(missingFromREADME)
	if len(missingFromManifest) > 0 {
		t.Errorf("README.md links these (tag, file) pairs but demo-videos.ts EPISODES has no matching shipped entry: %v", missingFromManifest)
	}
	if len(missingFromREADME) > 0 {
		t.Errorf("demo-videos.ts EPISODES ships these (tag, file) pairs but README.md links none of them: %v", missingFromREADME)
	}
}
