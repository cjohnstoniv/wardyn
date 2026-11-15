// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A scan that could not read part of the tree must not report high confidence over what it saw:
// the profile is incomplete and low confidence, and a source scan then keeps the requirement rows
// an earlier scan found.
func TestScan_UnreadablePartsMarkTheScanIncomplete(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind the scanning user")
	}
	for name, unreadable := range map[string]string{"a directory": "secrets", "a file": "secrets/.env.example"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "go.mod", "module x\n\ngo 1.22\n")
			writeFile(t, root, "secrets/.env.example", "HIDDEN_TOKEN=\n")
			p := filepath.Join(root, unreadable)
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
			got := Scan(root)
			if !got.Incomplete || got.Confidence != ConfidenceLow || !got.NeedsReview {
				t.Errorf("unreadable %s: incomplete %v, confidence %q, needs review %v; want true, low, true",
					unreadable, got.Incomplete, got.Confidence, got.NeedsReview)
			}
		})
	}
}

// A detector file is read up to its 1 MiB cap, which ends the read like EOF: a file that goes on
// past the cap marks the scan incomplete, and one that ends exactly at the cap does not.
func TestScan_ADetectorFilePastTheReadLimitMarksTheScanIncomplete(t *testing.T) {
	filler := strings.Repeat("# "+strings.Repeat("x", 1021)+"\n", maxFileBytes/1024) // 1024-byte lines, maxFileBytes in all
	// package.json is read whole by its own detector and line by line by the leak lane, which marks
	// the cut; lines under the 64 KiB line cap keep the overlong-line rule out of it.
	var pkg strings.Builder
	pkg.WriteString(`{"scripts":{"build":"make"}`)
	for i := 0; pkg.Len() <= maxFileBytes; i++ {
		fmt.Fprintf(&pkg, ",\n\"p%04d\":\"%s\"", i, strings.Repeat("x", 1000))
	}
	pkg.WriteString("}\n")
	for name, tc := range map[string]struct {
		file, body string
		incomplete bool
	}{
		"a template that ends at the cap":       {".env.example", filler, false},
		"a template whose last name is past it": {".env.example", filler + "LATE_TOKEN=\n", true},
		"a package.json longer than the cap":    {"package.json", pkg.String(), true},
		"a package.json well inside the cap":    {"package.json", `{"scripts":{"build":"make"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, tc.file, tc.body)
			got := Scan(root)
			if got.Incomplete != tc.incomplete || (got.Confidence == ConfidenceLow) != tc.incomplete {
				t.Errorf("incomplete %v, confidence %q; want incomplete %v", got.Incomplete, got.Confidence, tc.incomplete)
			}
		})
	}
}

// A list cut at its final cap is incomplete too, and the secret-name cap says how many names it
// left out: credential-declaring names first, then code and CI references.
func TestDeriveProfile_AListCutAtItsCapIsCountedAndIncomplete(t *testing.T) {
	needs := func(prefix, kind string, n int) []SecretNeed {
		var out []SecretNeed
		for i := 0; i < n; i++ {
			out = append(out, SecretNeed{Name: fmt.Sprintf("%s%03d", prefix, i), Kind: kind})
		}
		return out
	}
	list := func(format string, n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf(format, i))
		}
		return out
	}
	for name, tc := range map[string]struct {
		facts   ScanFacts
		omitted int
	}{
		"secret names past the cap":        {ScanFacts{SecretRequirements: needs("STRONG_", "generic", maxSecretNeeds+37)}, 37},
		"code references past a full list": {ScanFacts{SecretRequirements: append(needs("STRONG_", "generic", maxSecretNeeds), needs("CODE_", "code", 5)...)}, 5},
		"suggested hosts past the cap":     {ScanFacts{SuggestedEgress: list("h%d.example.com", maxSuggested+6)}, 0},
		"secret files past the cap":        {ScanFacts{SecretFilesPresent: list("dir%d/.env", maxSecretFiles+3)}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			got := DeriveProfile(tc.facts)
			if !got.Incomplete || got.Confidence != ConfidenceLow || got.RequiredSecretsOmitted != tc.omitted {
				t.Errorf("incomplete %v, confidence %q, omitted %d; want true, low, %d",
					got.Incomplete, got.Confidence, got.RequiredSecretsOmitted, tc.omitted)
			}
		})
	}
	full := DeriveProfile(ScanFacts{SecretRequirements: needs("STRONG_", "generic", maxSecretNeeds)})
	if full.Incomplete || full.Confidence != ConfidenceHigh || full.RequiredSecretsOmitted != 0 || len(full.RequiredSecrets) != maxSecretNeeds {
		t.Errorf("a list exactly at its cap: incomplete %v, confidence %q, omitted %d, kept %d; want false, high, 0, %d",
			full.Incomplete, full.Confidence, full.RequiredSecretsOmitted, len(full.RequiredSecrets), maxSecretNeeds)
	}
	// Services carry no cut flag: their ids come from serviceByImagePrefix, fewer than the cap.
	ids := map[string]struct{}{}
	for _, id := range serviceByImagePrefix {
		ids[id] = struct{}{}
	}
	if len(ids) > maxServices {
		t.Errorf("%d service ids, over the %d cap: validateServices can now cut, so it needs the incomplete signal", len(ids), maxServices)
	}
}

// A workspace's merged profile is incomplete when any source's is, and sums the omitted counts.
func TestMergeProfiles_KeepsIncompleteAndSumsOmittedNames(t *testing.T) {
	got := MergeProfiles([]WorkspaceProfile{
		{Confidence: ConfidenceLow, Incomplete: true, RequiredSecretsOmitted: 3},
		{Confidence: ConfidenceHigh},
		{Confidence: ConfidenceLow, Incomplete: true, RequiredSecretsOmitted: 4},
	}, []string{"a", "b", "c"}, "a")
	if !got.Incomplete || got.RequiredSecretsOmitted != 7 || got.Confidence != ConfidenceLow {
		t.Errorf("merged: incomplete %v, omitted %d, confidence %q; want true, 7, low", got.Incomplete, got.RequiredSecretsOmitted, got.Confidence)
	}
}

// A scan budget lowers confidence and leaves the profile complete, so a source scan of a repo past
// it still drops a scan-found row a rescan no longer finds. An unreadable entry makes the profile
// incomplete, so the scan keeps the earlier rows. One signal for both fails one of the cases.
func TestScan_ABudgetStopIsLowConfidenceAndAnUnreadEntryIsIncomplete(t *testing.T) {
	for name, tc := range map[string]struct {
		setup      func(t *testing.T, root string)
		incomplete bool
	}{
		"past the line budget": {func(t *testing.T, root string) {
			writeFile(t, root, ".env.example", "NEW_TOKEN=\n"+strings.Repeat("\n", maxDetectLines))
		}, false},
		"past the source-file budget": {func(t *testing.T, root string) {
			for i := 0; i <= maxSourceFilesScanned; i++ {
				writeFile(t, root, fmt.Sprintf("src/f%03d.go", i), "package src\n")
			}
		}, false},
		"an unreadable folder": {func(t *testing.T, root string) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("needs POSIX permissions that bind the scanning user")
			}
			writeFile(t, root, "secrets/.env.example", "HIDDEN_TOKEN=\n")
			p := filepath.Join(root, "secrets")
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got := Scan(root)
			if got.Incomplete != tc.incomplete || got.Confidence != ConfidenceLow {
				t.Errorf("incomplete %v, confidence %q; want incomplete %v, low", got.Incomplete, got.Confidence, tc.incomplete)
			}
		})
	}
}

// The advisor's hosts go through the same 64-host cap as the scan's: a host the cap leaves out,
// of the advice alone or of the merged list, marks the profile incomplete.
func TestMergeAdvice_AHostTheCapLeavesOutMarksTheProfileIncomplete(t *testing.T) {
	hosts := func(prefix string, n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("%s%02d.example.com", prefix, i))
		}
		return out
	}
	for name, tc := range map[string]struct {
		scanned, advised int
		incomplete       bool
	}{
		"60 scanned and 6 advised": {60, 6, true},
		"58 scanned and 6 advised": {58, 6, false},
		"70 advised":               {0, maxSuggested + 6, true},
	} {
		t.Run(name, func(t *testing.T) {
			base := WorkspaceProfile{SuggestedEgress: hosts("s", tc.scanned), Confidence: ConfidenceHigh, Source: SourceDeterministic}
			got := mergeAdvice(base, adviceWire{EgressDomains: hosts("a", tc.advised)})
			if got.Incomplete != tc.incomplete || (got.Confidence == ConfidenceLow) != tc.incomplete ||
				len(got.SuggestedEgress) != min(tc.scanned+tc.advised, maxSuggested) {
				t.Errorf("incomplete %v, confidence %q, %d hosts; want incomplete %v and %d hosts",
					got.Incomplete, got.Confidence, len(got.SuggestedEgress), tc.incomplete, min(tc.scanned+tc.advised, maxSuggested))
			}
		})
	}
}
