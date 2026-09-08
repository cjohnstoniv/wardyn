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

// B7 (R3 scan-git fix-up round 4, RE-DERIVED in the adversarial fix-up round):
// THREAT-MODEL.md 5.1a's findings-cap bullet must state what the cap actually
// does, and the code moved under it.
//
// Round 4's claim was "the severity keep-back applies under `mode=block` ONLY;
// under `mode=alert` findings past the cap are dropped regardless of severity"
// — true of the code as it then stood, and the reason this guard exists. The
// adversarial round found that behaviour is itself the defect (F075): alert
// mode's only product IS the alert, and 900 cheap low-severity findings evicted
// the operator's high-severity one from it. internal/contentscan now applies
// severity priority in EVERY mode — block mode GROWS the report to a hard
// ceiling, every other mode DISPLACES a retained below-threshold finding — so
// the doc claim is re-derived here against the merged tree rather than skipped.
//
// The guard therefore asserts the CODE premise (the keep-back is not
// conditioned on ModeBlock) and the DOC's two disclosures: severity priority in
// every mode, and that findings below the threshold are still dropped, so the
// decision log remains truncated rather than complete.
func TestThreatModelDocFindingsCapKeepBackAppliesInEveryMode(t *testing.T) {
	// Premise: the keep-back arm no longer gates on the mode.
	src := readRepoFile(t, "internal/contentscan/contentscan.go")
	if strings.Contains(src, "if e.mode == ModeBlock && severityRank(f.Severity) >= severityRank(e.blockMin)") {
		t.Fatal("the findings-cap keep-back is gated on ModeBlock again — re-derive this doc claim " +
			"(and F075's alert-mode pin) before trusting this guard")
	}
	if !strings.Contains(src, "severityRank(kept[displace].Severity)") {
		t.Fatal("the non-block displacement arm is gone from ScanRequest's truncation — the doc's " +
			"\"survives in every mode\" claim would no longer be true")
	}

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
	for _, want := range []string{
		"in EVERY mode", // the keep-back's scope
		"mode=block",    // ... and how it differs per mode
		"mode=alert",
		"DISPLACES",
		"truncated, not complete", // the honest ceiling that survives the fix
	} {
		if !strings.Contains(bullet, want) {
			t.Fatalf("5.1a's findings-cap bullet no longer says %q — it must state that a finding at or "+
				"above `block_min_severity` survives the cap in every mode (block by keeping it past the "+
				"cap, alert by displacing a lower-severity one) AND that lower-severity findings past the "+
				"cap are still dropped, so the decision log is truncated. Bullet text:\n%s", want, bullet)
		}
	}
	// The BYTE bound is part of the same claim: the cap alone does not bound the
	// decision log's size (F075's amplification finding).
	if !strings.Contains(bullet, "field_path") || !strings.Contains(bullet, "sanitizePath") {
		t.Fatal("5.1a's findings-cap bullet must also disclose the per-finding field_path bound " +
			"(internal/contentscan/patterns.go, sanitizePath): the findings cap bounds the NUMBER of " +
			"findings, and without the byte bound a 0.3 MiB body of long agent-authored keys still " +
			"produced a 46 MB decision log under a cap that never fired")
	}
}
