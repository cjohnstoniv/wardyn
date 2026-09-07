// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cappedBodyJWTsThenAKIAs builds the body the findings cap is measured on: n
// SevMedium JWT matches followed by m SevHigh AWS-key matches, one match per
// leaf, in that order. The regex catalog is the only detector, so the finding
// count is EXACT (n+m) rather than an entropy estimate, and the block-relevant
// matches arrive AFTER the cap has already fired — which is what makes them
// keep-backs.
func cappedBodyJWTsThenAKIAs(n, m int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"leaves":[`)
	for i := range n {
		if i > 0 {
			sb.WriteString(",")
		}
		// A syntactically valid JWT shape (SevMedium), distinct per leaf.
		fmt.Fprintf(&sb, `"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ%08d.sig%08dPAYLOAD"`, i, i)
	}
	for i := range m {
		fmt.Fprintf(&sb, `,"AKIA%016d"`, i)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// patternsEngine builds a regex-catalog-only engine so every finding's severity
// is a table constant rather than an entropy score.
func patternsEngine(t *testing.T, mode, blockMin string) *Engine {
	t.Helper()
	eng, err := NewEngine(types.LLMInspectionSpec{
		Mode: mode, DetectSecretPatterns: true, BlockMinSeverity: blockMin,
	}, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if eng == nil {
		t.Fatal("expected a non-nil engine")
	}
	return eng
}

// TestFindingsSeenIsTheCountedPreCapTotalInBlockMode pins Result.FindingsSeen
// to the number of findings the scan ACTUALLY produced, measured against an
// uncapped control engine rather than inferred from the surviving report.
//
// The inference it replaces — len(Findings) + FindingsDropped, which
// scanSummaryFrom used for the wire's findings_total — is wrong in exactly the
// mode where the audit row is enforcement evidence. FindingsDropped counts every
// finding EXAMINED past the cap, and under ModeBlock capFindings then keeps the
// block-relevant ones back INTO Findings, so each keep-back is counted on both
// sides of that sum. Measured on this fixture: reported=800, past-cap=700, so
// the sum says 1,500 for a body that contains 1,200 findings.
//
// The proxy half of the same claim (the findings_total field an auditor reads)
// is pinned end-to-end in TestScanSummaryFindingsTotalIsCountedNotInferred,
// internal/egress/proxy — this half is where an UNCAPPED control engine can be
// built at all, since the cap is a built-in constant and not policy.
func TestFindingsSeenIsTheCountedPreCapTotalInBlockMode(t *testing.T) {
	const (
		medium = 900 // below block_min_severity=high: dropped past the cap
		high   = 300 // at/above it: kept back INTO the report, and still counted dropped
		total  = medium + high
	)
	body := cappedBodyJWTsThenAKIAs(medium, high)

	// The control: the same engine with the cap OFF, so its report IS the truth.
	control := patternsEngine(t, "block", "high")
	control.maxFindings = 0
	ctl, _, err := control.ScanRequest(ChannelGeneric, body)
	if err != nil {
		t.Fatalf("control scan: %v", err)
	}
	if len(ctl.Findings) != total {
		t.Fatalf("fixture produced %d findings uncapped, want %d — the fixture, not the cap, is wrong",
			len(ctl.Findings), total)
	}
	if ctl.FindingsCapped || ctl.FindingsDropped != 0 {
		t.Fatalf("control engine truncated (capped=%v dropped=%d): it must not",
			ctl.FindingsCapped, ctl.FindingsDropped)
	}
	if ctl.FindingsSeen != total {
		t.Fatalf("uncapped FindingsSeen = %d, want %d: the counter must agree with the report when "+
			"nothing was truncated", ctl.FindingsSeen, total)
	}

	eng := patternsEngine(t, "block", "high")
	res, _, err := eng.ScanRequest(ChannelGeneric, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.FindingsCapped {
		t.Fatal("the cap did not fire — this fixture must truncate, or it pins nothing")
	}
	if len(res.Findings) <= eng.maxFindings {
		t.Fatalf("reported %d findings with a cap of %d: block mode must have kept the block-relevant "+
			"past-cap findings back, or there is no double-count to measure",
			len(res.Findings), eng.maxFindings)
	}
	if res.FindingsSeen != total {
		t.Fatalf("FindingsSeen = %d, want %d (the uncapped control's report): the pre-cap total must be "+
			"COUNTED as findings are produced — reported=%d past-cap=%d, whose sum is %d",
			res.FindingsSeen, total, len(res.Findings), res.FindingsDropped,
			len(res.Findings)+res.FindingsDropped)
	}
	if res.FindingsSeen != len(ctl.Findings) {
		t.Fatalf("FindingsSeen = %d but the uncapped control reported %d", res.FindingsSeen, len(ctl.Findings))
	}
	t.Logf("capped: reported=%d past-cap=%d seen=%d; the old inferred sum would be %d",
		len(res.Findings), res.FindingsDropped, res.FindingsSeen,
		len(res.Findings)+res.FindingsDropped)
}
