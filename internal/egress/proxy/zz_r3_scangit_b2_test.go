// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestScanSummaryFrom_FindingsCappedStillAlerts pins B2 (F075 fix-up):
// findings_capped sets Result.Skipped, and scanSummaryFrom used to resolve
// `case res.Skipped` BEFORE ever reaching the "alert" default — so the
// decision's Action flipped from "alert" to "skipped" exactly when the scan
// produced the MOST findings. egress.ScanSummary.Action is the literal
// audit-action suffix (docs/AUDIT-ACTIONS.md:71: llm.scan.alert vs
// llm.scan.skipped), so a 500-finding capped body used to audit as
// llm.scan.skipped instead of llm.scan.alert.
//
// A capped scan with findings must still alert; an uncapped scan with the
// same finding must also alert (baseline); and a finding-free skip (e.g.
// span_oversize) must still read as "skipped", not "alert".
func TestScanSummaryFrom_FindingsCappedStillAlerts(t *testing.T) {
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true,
	}, [][]byte{[]byte("sk_live_test_corpus_secret_00000000000000")})
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	oneFinding := []contentscan.Finding{{Detector: "known-secret", FieldPath: "messages[0]"}}

	capped := contentscan.Result{Scanned: true, Findings: oneFinding, Skipped: true, SkipReason: "findings_capped"}
	if got := scanSummaryFrom(capped, nil, eng, "", contentscan.ChannelAnthropicMessages).Action; got != "alert" {
		t.Errorf("findings_capped with %d finding(s): Action = %q, want %q (the capped scan is the LOUDEST scan there is)",
			len(oneFinding), got, "alert")
	}

	uncapped := contentscan.Result{Scanned: true, Findings: oneFinding}
	if got := scanSummaryFrom(uncapped, nil, eng, "", contentscan.ChannelAnthropicMessages).Action; got != "alert" {
		t.Errorf("uncapped baseline: Action = %q, want %q", got, "alert")
	}

	oversize := contentscan.Result{Scanned: true, Skipped: true, SkipReason: "span_oversize"}
	if got := scanSummaryFrom(oversize, nil, eng, "", contentscan.ChannelAnthropicMessages).Action; got != "skipped" {
		t.Errorf("finding-free span_oversize: Action = %q, want %q (must still read as skipped)", got, "skipped")
	}
}

// TestScanSummaryFrom_SkipReasonsWithFindingsStillAlert pins B4 (F073/F056
// fix-up): B2 fixed findings_capped only, but scan_budget (F073) and
// attachment_decode_error (F056) fall into the identical `case res.Skipped`
// arm and flip a genuinely finding-bearing scan's audit action from
// llm.scan.alert to llm.scan.skipped — the same sibling-caller class B2
// declared blocking, with two of three siblings missed.
func TestScanSummaryFrom_SkipReasonsWithFindingsStillAlert(t *testing.T) {
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true,
	}, [][]byte{[]byte("sk_live_test_corpus_secret_00000000000000")})
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	oneFinding := []contentscan.Finding{{Detector: "known-secret", FieldPath: "messages[0]"}}

	for _, reason := range []string{"findings_capped", "scan_budget", "attachment_decode_error"} {
		r := contentscan.Result{Scanned: true, Findings: oneFinding, Skipped: true, SkipReason: reason}
		if got := scanSummaryFrom(r, nil, eng, "", contentscan.ChannelAnthropicMessages).Action; got != "alert" {
			t.Errorf("%s with a finding: Action = %q, want %q (a detected secret must still audit as llm.scan.alert)", reason, got, "alert")
		}
	}
}
