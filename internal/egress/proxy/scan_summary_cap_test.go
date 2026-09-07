// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestScanSummaryPutsTheTruncationOnTheWire pins the F075 fix-up's third and
// fourth items: a capped scan has to be DISTINGUISHABLE, in the audit, from one
// that happened to find exactly the cap's worth of findings.
//
// egress.ScanSummary carried only the (already truncated) findings array, so
// the wire said nothing about the cap; and when an earlier skip reason had
// claimed SkipReason, nothing said the result was truncated at all.
func TestScanSummaryPutsTheTruncationOnTheWire(t *testing.T) {
	eng := scanEngine(t, "alert", scanTestSecret)

	res := contentscan.Result{
		Scanned:         true,
		Findings:        []contentscan.Finding{{Detector: "known-secret", Severity: contentscan.SevHigh}},
		Skipped:         true,
		SkipReason:      "span_oversize", // an EARLIER reason won the single field
		FindingsDropped: 401,
		FindingsSeen:    402,
		FindingsCapped:  true,
	}
	s := scanSummaryFrom(res, nil, eng, "", contentscan.ChannelGeneric)
	if !s.FindingsCapped {
		t.Fatal("ScanSummary.FindingsCapped = false behind an earlier skip reason: the truncation must " +
			"ride on its own flag, not on the single-valued skip_reason")
	}
	if s.FindingsPastCap != 401 {
		t.Fatalf("FindingsPastCap = %d, want 401 (findings examined past the cap)", s.FindingsPastCap)
	}
	// 402 is the fixture's COUNTED pre-cap total (Result.FindingsSeen), not
	// reported+past-cap: those coincide here only because this synthetic result
	// has no block-mode keep-back. TestScanSummaryFindingsTotalIsCountedNotInferred
	// below is the case where they differ, and it is the one that runs a real scan.
	if s.FindingsTotal != 402 {
		t.Fatalf("FindingsTotal = %d, want 402: an auditor comparing what was FOUND "+
			"against what was REPORTED needs the pre-cap total", s.FindingsTotal)
	}
	if s.Action != "alert" {
		t.Fatalf("Action = %q, want alert: a truncated scan that still carries a finding is the loudest "+
			"scan there is", s.Action)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"findings_capped":true`, `"findings_past_cap":401`, `"findings_total":402`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("ScanSummary JSON %s is missing %s — the control plane cannot record what the "+
				"proxy does not send", b, want)
		}
	}

	// A scan that was NOT capped must stay byte-identical to before: the three
	// fields are omitempty so no ordinary row grows.
	clean := scanSummaryFrom(contentscan.Result{
		Scanned:  true,
		Findings: []contentscan.Finding{{Detector: "known-secret", Severity: contentscan.SevHigh}},
	}, nil, eng, "", contentscan.ChannelGeneric)
	cb, _ := json.Marshal(clean)
	for _, unwanted := range []string{"findings_capped", "findings_past_cap", "findings_total"} {
		if strings.Contains(string(cb), unwanted) {
			t.Fatalf("an uncapped ScanSummary carries %q: %s", unwanted, cb)
		}
	}
	_ = egress.ScanSummary{}
}

// scanCapBodyJWTsThenAKIAs builds n SevMedium JWT matches followed by m SevHigh
// AWS-key matches, one match per leaf, in that order: the regex catalog is the
// only detector, so the finding count is EXACT and the block-relevant matches
// arrive after the cap has already fired.
func scanCapBodyJWTsThenAKIAs(n, m int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"leaves":[`)
	for i := range n {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ%08d.sig%08dPAYLOAD"`, i, i)
	}
	for i := range m {
		fmt.Fprintf(&sb, `,"AKIA%016d"`, i)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// TestScanSummaryFindingsTotalIsCountedNotInferred runs a REAL scan end to end
// —Engine.ScanRequest in mode=block, cap fired, severity keep-backs retained —
// and pins the number an auditor reads out of the audit row.
//
// TestScanSummaryPutsTheTruncationOnTheWire above hand-builds a
// contentscan.Result and so cannot see this: findings_total was computed as
// len(Findings) + FindingsDropped, and FindingsDropped counts every finding
// EXAMINED past the cap — including the block-relevant ones capFindings then
// keeps back INTO Findings. Every keep-back was therefore counted twice.
// Measured on this fixture: 900 medium findings then 300 high ones under
// block_min_severity=high report 800 rows with 700 past the cap, so the sum
// says 1,500 for a body that contains 1,200 findings. Block mode is precisely
// where that row is enforcement evidence.
func TestScanSummaryFindingsTotalIsCountedNotInferred(t *testing.T) {
	const (
		medium        = 900
		high          = 300
		total         = medium + high
		wantReported  = 800 // 500 retained + 300 severity keep-backs
		wantPastCap   = 700 // 400 dropped + the 300 keep-backs, which are counted too
		inferredWrong = wantReported + wantPastCap
	)
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{
		Mode: "block", DetectSecretPatterns: true, BlockMinSeverity: "high",
	}, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, _, err := eng.ScanRequest(contentscan.ChannelGeneric, scanCapBodyJWTsThenAKIAs(medium, high))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	s := scanSummaryFrom(res, nil, eng, "", contentscan.ChannelGeneric)
	if !s.FindingsCapped {
		t.Fatal("the cap did not fire — this fixture must truncate, or it pins nothing")
	}
	if len(s.Findings) != wantReported || s.FindingsPastCap != wantPastCap {
		t.Fatalf("reported=%d past_cap=%d, want %d/%d: the fixture must be the shape that keeps "+
			"block-relevant findings back past the cap", len(s.Findings), s.FindingsPastCap,
			wantReported, wantPastCap)
	}
	if s.FindingsTotal != total {
		t.Fatalf("findings_total = %d, want %d: the audit row's pre-cap total must be COUNTED as the "+
			"scan produces findings, not inferred as reported+past_cap (%d) — block mode reports a "+
			"past-cap finding it kept back for severity while ALSO counting it past the cap, so that "+
			"sum double-counts every keep-back", s.FindingsTotal, total, inferredWrong)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"findings_total":1200`) {
		t.Fatalf("ScanSummary JSON does not carry findings_total=1200: %s", b[:min(len(b), 400)])
	}
}
