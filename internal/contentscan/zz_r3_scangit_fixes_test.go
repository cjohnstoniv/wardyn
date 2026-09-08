// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestF075_FindingsCapBoundsRequest pins F075: nothing capped how many
// Findings one ScanRequest could return, so a body split into many spans (a
// JSON connector body with many string leaves, in this test) fanned out into
// one finding per span with no upper bound — every one destined to be copied
// verbatim into the decision log. The fix must cap the result and report the
// truncation honestly via Skipped/SkipReason, the same shape as every other
// "content not fully inspected" case.
func TestF075_FindingsCapBoundsRequest(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	const n = 700 // comfortably above the intended few-hundred cap
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf(`"k%d":"leak %s"`, i, testSecret))
	}
	sb.WriteString("}")

	res, _, err := eng.ScanRequest(ChannelGeneric, []byte(sb.String()))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) >= n {
		t.Fatalf("expected a per-request finding cap well under the %d possible findings, got %d (skipped=%v reason=%q)",
			n, len(res.Findings), res.Skipped, res.SkipReason)
	}
	if !res.Skipped || res.SkipReason != "findings_capped" {
		t.Fatalf("expected Skipped{findings_capped} once the cap is hit, got skipped=%v reason=%q findings=%d",
			res.Skipped, res.SkipReason, len(res.Findings))
	}
}

// TestF073_ScanBudgetStopsFurtherScanning pins F073: the per-span
// max_scan_bytes cap does nothing to bound the TOTAL bytes scanned across a
// request's many sub-cap spans. This test places ten 500,000-byte padding
// blocks (5,000,000 bytes total, each individually well under the 1 MiB
// per-span cap) before a final block carrying a known secret, in a
// deterministically-ordered Anthropic content-blocks array. Once a
// per-request scan budget is enforced, the secret block — reached only after
// the budget is exhausted — must NOT be scanned at all: this proves scanning
// actually STOPPED (bounding the CPU cost), not merely that a flag got set.
func TestF073_ScanBudgetStopsFurtherScanning(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	pad := strings.Repeat("A", 500000) // well under the 1 MiB per-span cap
	var blocks []string
	for i := 0; i < 10; i++ {
		blocks = append(blocks, `{"type":"text","text":"`+pad+`"}`)
	}
	blocks = append(blocks, `{"type":"text","text":"leak `+testSecret+`"}`)
	body := []byte(`{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":[` +
		strings.Join(blocks, ",") + `]}]}`)

	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.Skipped || res.SkipReason != "scan_budget" {
		t.Fatalf("expected Skipped{scan_budget} once the 5,000,000-byte total (> the 4 MiB default budget) is exhausted, got skipped=%v reason=%q findings=%d",
			res.Skipped, res.SkipReason, len(res.Findings))
	}
	if len(res.Findings) != 0 {
		t.Fatalf("the secret block placed after the exhausted scan budget must NOT be scanned, but got findings: %+v", res.Findings)
	}
}

// TestF056_AttachmentDecodeFailureRecordedHonestly pins F056: a base64
// attachment block extractAnthropicAttachments could not decode used to be
// silently dropped (bare `continue`, no Skipped/SkipReason) and the result
// still came back Scanned=true/Skipped=false — an undecodable-but-secret-
// carrying attachment passed as "inspected clean" even under block +
// on_scanner_error=block. The fix must (a) also try the non-StdEncoding
// alphabets a real client may use before declaring failure, and (b) report a
// genuine failure honestly so block+on_scanner_error=block refuses it.
func TestF056_AttachmentDecodeFailureRecordedHonestly(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{
		Mode: "block", DetectSecrets: true, ScanAttachments: true, OnScannerError: "block",
	}, [][]byte{[]byte(testSecret)})
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// Unpadded (RawStdEncoding) base64: base64.StdEncoding.DecodeString alone
	// rejects it, but it is a perfectly well-formed attachment.
	raw := base64.RawStdEncoding.EncodeToString([]byte("token=" + testSecret))
	body := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"user","content":` +
		`[{"type":"document","source":{"type":"base64","data":"` + raw + `"}}]}]}`)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("an unpadded-base64 attachment must still be decoded and scanned, got no findings")
	}

	// A block that is not valid base64 under ANY known alphabet is a genuine
	// decode failure: must be Skipped{attachment_decode_error}, not "clean".
	bogus := "not-valid-base64!!!"
	body2 := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"user","content":` +
		`[{"type":"document","source":{"type":"base64","data":"` + bogus + `"}}]}]}`)
	res2, _, err := eng.ScanRequest(ChannelAnthropicMessages, body2)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res2.Skipped || res2.SkipReason != "attachment_decode_error" {
		t.Fatalf("expected Skipped{attachment_decode_error} for an undecodable attachment, got skipped=%v reason=%q scanned=%v findings=%+v",
			res2.Skipped, res2.SkipReason, res2.Scanned, res2.Findings)
	}
	if !eng.ShouldBlock(res2) {
		t.Fatal("an undecodable attachment under block+on_scanner_error=block must refuse the request, not pass as clean")
	}
}

// TestF049_OpenAIChatScansSystemPromptMessage pins F049: THREAT-MODEL.md
// 5.1a's "Only the system prompt + the last message of each turn are
// scanned" is true for the Anthropic channel (top-level `system` field) but
// was false for OpenAI/Codex, which carries the system prompt as a
// role:"system" MESSAGE — extractOpenAIChat read only the last message, so a
// system prompt was scanned only in the degenerate case where it was ALSO
// the last message. This places the secret in a system message that is NOT
// the last message.
func TestF049_OpenAIChatScansSystemPromptMessage(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	body := `{"model":"gpt-4","messages":[` +
		`{"role":"system","content":` + jsonString("system secret "+testSecret) + `},` +
		`{"role":"user","content":"just an ordinary question"}]}`
	res, _, err := eng.ScanRequest(ChannelOpenAIChat, []byte(body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("a secret in the OpenAI system-role message (not the last message) must still be scanned, got no findings")
	}
}

// TestF075_FindingsCapDoesNotSuppressBlocking pins B5: the per-request
// finding cap bounds what a scan REPORTS, never what block mode ENFORCES. An
// agent that fans out cheap noise ahead of its real secret must not be able
// to buy a forward that the base tree refused. This mirrors review2's
// reproduction (b): mode=block, detect_entropy (medium-severity noise) +
// detect_secret_patterns, block_min_severity=high; 900 entropy-noise blocks
// then an AKIA... access-key-id (high) — the base tree (and a naive
// scan-stopping cap) forwards this because the cap fires on cheap noise
// before the real secret is ever scanned; the fix must still block.
func TestF075_FindingsCapDoesNotSuppressBlocking(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{
		Mode: "block", DetectEntropy: true, DetectSecretPatterns: true, BlockMinSeverity: "high",
	}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	var blocks []string
	for i := 0; i < 900; i++ {
		blocks = append(blocks, fmt.Sprintf(`{"type":"text","text":"tok%04d aHR0cHM6Ly9leGFtcGxlLmNvbS9hYmNkZWZnaGlqa2xtbm9wcXJz%04d"}`, i, i))
	}
	blocks = append(blocks, `{"type":"text","text":"AKIAIOSFODNN7EXAMPLE"}`)
	body := []byte(`{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":[` +
		strings.Join(blocks, ",") + `]}]}`)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) > 2*defaultMaxFindings {
		t.Fatalf("the reporting cap must still bound the decision log, got %d findings", len(res.Findings))
	}
	if !eng.ShouldBlock(res) {
		t.Fatalf("a high-severity key behind %d cheap findings must still block under block_min_severity=high, got skipped=%v reason=%q findings=%d",
			900, res.Skipped, res.SkipReason, len(res.Findings))
	}
}

// TestB6_FindingsCapTruncationArmExaminesEachFindingOnce pins B6: B5's
// keep-back arm re-slices `res.Findings[:maxFindings]` and re-walks the
// retained tail on EVERY subsequent span once the cap first fires (the
// retained tail sits between maxFindings and 2*maxFindings, so
// `len(res.Findings) > e.maxFindings` stays true on every later span), making
// the arm's own cost O(span_count x maxFindings) with span_count unbounded by
// the scan_budget (which counts scanned TEXT, not span count). The visible
// symptom is Result.FindingsDropped re-counting the SAME already-vetted
// findings once per remaining span instead of counting each dropped finding
// exactly once. This places 2000 SevLow email (PII) findings — every one
// block-relevant under the default block_min_severity=low, so the old code's
// keep-back exemption applies to all of them and the re-walk fires on every
// one of the ~1500 spans after the cap trips — followed by one SevHigh
// AKIA... access-key-id finding, under ModeBlock with DetectPII +
// DetectSecretPatterns and default block_min_severity.
func TestB6_FindingsCapTruncationArmExaminesEachFindingOnce(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{
		Mode: "block", DetectPII: true, DetectSecretPatterns: true,
	}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	var blocks []string
	for i := 0; i < 2000; i++ {
		blocks = append(blocks, fmt.Sprintf(`{"type":"text","text":"contact user%04d@example.com for details"}`, i))
	}
	blocks = append(blocks, `{"type":"text","text":"AKIAIOSFODNN7EXAMPLE"}`)
	body := []byte(`{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":[` +
		strings.Join(blocks, ",") + `]}]}`)

	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !eng.ShouldBlock(res) {
		t.Fatalf("a high-severity key behind 2000 low-severity PII findings must still block under "+
			"the default block_min_severity, got skipped=%v reason=%q findings=%d",
			res.Skipped, res.SkipReason, len(res.Findings))
	}
	if res.FindingsDropped > 4*defaultMaxFindings {
		t.Fatalf("FindingsDropped must count each finding examined past the cap exactly ONCE "+
			"(one pass over the tail), not re-count the same already-vetted findings on every "+
			"subsequent span's re-slice/re-walk of res.Findings[:maxFindings] — got %d, "+
			"want <= %d (4x the cap)", res.FindingsDropped, 4*defaultMaxFindings)
	}
}
