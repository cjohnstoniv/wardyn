// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// B1 (R3 scan-git fix wave): THREAT-MODEL.md 5.1a's coverage-gap bullet said
// the primary inadvertent-leak paths (a fresh paste, a tool_result of a
// just-read file) "are the last element and are covered" without disclosing
// that the per-request scan_budget (F073) can leave the REST of that same
// newest message unscanned once the budget is exhausted — the exact
// overstated-coverage class F049 in this same lane exists to fix. This guard
// fails if that caveat, or the budget size it must name, ever goes missing.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// maxTotalScanBytesConst matches the built-in per-request scan-budget constant
// in internal/contentscan/contentscan.go: `const defaultMaxTotalScanBytes = N
// << 20 // <MiB> MiB`. Reading the CODE (not hardcoding the MiB figure) means
// a future change to the budget reddens this guard instead of silently
// leaving the doc's number stale.
var maxTotalScanBytesConst = regexp.MustCompile(
	`(?m)^const defaultMaxTotalScanBytes = (\d+) << 20 // (\d+) MiB`)

func TestThreatModelDocScanBudgetCaveatPresent(t *testing.T) {
	code := readRepoFile(t, "internal/contentscan/contentscan.go")
	m := maxTotalScanBytesConst.FindStringSubmatch(code)
	if m == nil {
		t.Fatal("defaultMaxTotalScanBytes const not found (or its shape changed) in " +
			"internal/contentscan/contentscan.go — the guard's anchor is gone")
	}
	multiplier, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse multiplier %q: %v", m[1], err)
	}
	mib, err := strconv.Atoi(m[2])
	if err != nil {
		t.Fatalf("parse MiB comment %q: %v", m[2], err)
	}
	if multiplier != mib {
		t.Fatalf("defaultMaxTotalScanBytes = %d << 20 does not equal its own %d MiB comment — fix the source before trusting either", multiplier, mib)
	}

	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	idx := strings.Index(doc, "Only the **system prompt + the last message**")
	if idx < 0 {
		t.Fatal("5.1a's system-prompt-plus-last-message coverage bullet moved or was removed — re-point this guard")
	}
	// Bound the search to this one bullet (up to the next top-level "- " bullet)
	// so a caveat added to a LATER bullet does not count for this one.
	bullet := doc[idx:]
	if end := strings.Index(bullet, "\n- A single span over"); end > 0 {
		bullet = bullet[:end]
	}
	if strings.Contains(bullet, "are the last element and are covered.") {
		t.Fatal("5.1a's coverage-gap bullet still claims the primary inadvertent-leak paths " +
			"\"are the last element and are covered\" with no caveat — the per-request scan_budget " +
			"(F073) can leave the REST of that same newest message unscanned once exhausted, which is " +
			"exactly the overstated-coverage class F049 in this lane exists to fix (B1)")
	}
	wantMiB := strconv.Itoa(mib) + " MiB"
	if !strings.Contains(bullet, wantMiB) || !strings.Contains(bullet, "goes unscanned too") {
		t.Fatalf("5.1a's coverage-gap bullet must disclose the scan_budget caveat naming the actual "+
			"budget (%s) and that the rest of the newest message goes unscanned once it is exhausted; "+
			"bullet text:\n%s", wantMiB, bullet)
	}
}

// B7 (R3 scan-git fix-up round 4): THREAT-MODEL.md 5.1a's findings-cap bullet
// stated the severity keep-back UNCONDITIONALLY ("a finding AT OR ABOVE
// `block_min_severity` is kept ... and still blocks under `mode=block`" reads
// as a description of the cap's general behavior, not a mode=block-only one),
// but the code (internal/contentscan/contentscan.go) only keeps a
// block-relevant finding back in ModeBlock — in mode=alert every finding past
// the cap is dropped regardless of severity, so an operator running alert
// mode (5.1a's own baseline configuration) is told their critical finding
// survives truncation into the decision log when it does not. This guard
// fails if the bullet ever again describes the keep-back without scoping it
// to mode=block, or drops the alert-mode "dropped regardless of severity"
// disclosure.
func TestThreatModelDocFindingsCapKeepBackScopedToBlockMode(t *testing.T) {
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	idx := strings.Index(doc, "A separate **findings cap**")
	if idx < 0 {
		t.Fatal("5.1a's findings-cap bullet moved or was removed — re-point this guard")
	}
	// Bound the search to this one bullet (up to the next top-level "- " bullet)
	// so text in a different bullet does not count for this one.
	bullet := doc[idx:]
	if end := strings.Index(bullet, "\n- **Walled-garden coverage"); end > 0 {
		bullet = bullet[:end]
	}
	if !strings.Contains(bullet, "under `mode=block`") {
		t.Fatal("5.1a's findings-cap bullet must scope the severity keep-back to `mode=block` " +
			"explicitly — the code (contentscan.go) only exempts a block-relevant finding from the " +
			"cap when e.mode == ModeBlock; stating the keep-back without that scope tells an " +
			"alert-mode operator their critical finding survives truncation when it does not (B7)")
	}
	hasSeverityDisclosure := strings.Contains(bullet, "regardless of severity") ||
		strings.Contains(bullet, "REGARDLESS OF SEVERITY")
	if !strings.Contains(bullet, "mode=alert") || !hasSeverityDisclosure {
		t.Fatal("5.1a's findings-cap bullet must also disclose that under `mode=alert` (where nothing " +
			"is enforced) findings past the cap are dropped regardless of severity, so a high-finding-" +
			"count alert body's decision log is truncated, not complete (B7)")
	}
}
