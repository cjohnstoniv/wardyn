// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const mcpScanMarker = "CONFIDENTIAL//NOFORN"

// mcpScanEngine is the run's content inspection extended to forwarded egress, flagging mcpScanMarker.
func mcpScanEngine(t *testing.T, spec types.LLMInspectionSpec) *contentscan.Engine {
	t.Helper()
	spec.InspectForwardEgress = true
	if spec.DetectorSidecarURL == "" {
		spec.ClassifiedMarkers = []string{mcpScanMarker}
	}
	eng, err := contentscan.NewEngine(spec, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

// mcpMarkedCall is a tools/call of tool whose arguments carry mcpScanMarker.
func mcpMarkedCall(id int, tool string) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{"body": "notes " + mcpScanMarker}}})
	return string(b)
}

// wantScanRefused checks a body the content scan refused: a JSON-RPC error for id, nothing sent, the
// server's credential never resolved, and a scan:blocked row carrying the summary.
func wantScanRefused(t *testing.T, f *mcpFixture, rec *httptest.ResponseRecorder, id int) *egress.ScanSummary {
	t.Helper()
	var one struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || one.ID != id || one.Error.Code != mcpBlockedCode ||
		!strings.Contains(one.Error.Message, "content inspection") || f.up.hitCount() != 0 {
		t.Fatalf("answer %d %q, hits %d; want the content error for id %d and nothing sent", rec.Code, rec.Body, f.up.hitCount(), id)
	}
	if f.p.mcp.servers["linear"].header.value != "" {
		t.Fatal("the server's credential was resolved for a body the scan refused")
	}
	d := wantDecision(t, f.buf, egress.Deny, ruleSourceLLMBlocked)
	if d.Scan == nil || d.Scan.Action != "block" || d.Scan.Channel != string(contentscan.ChannelMCP) {
		t.Fatalf("deny row scan %+v, want a block summary on the mcp channel", d.Scan)
	}
	return d.Scan
}

// TestMCPBroker_ContentScanRefusesBeforeTheCredential: under inspect_forward_egress an allowed tool's
// call carrying a blocking marker is refused before the credential is resolved, on the direct path
// and on an approved call's rebuilt body.
func TestMCPBroker_ContentScanRefusesBeforeTheCredential(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__create_issue", Effect: types.ToolAllow}}}, nil, nil)
		f.p.scanner = mcpScanEngine(t, types.LLMInspectionSpec{Mode: "block"})
		if sc := wantScanRefused(t, f, f.post(t, mcpToken("codex-cli"), "linear", mcpMarkedCall(5, "create_issue")), 5); len(sc.Findings) == 0 {
			t.Fatalf("deny row scan %+v, want the marker finding", sc)
		}
	})
	t.Run("approved", func(t *testing.T) {
		f, cp := newEachFixture(t)
		cp.approveOnPoll = 1
		f.p.scanner = mcpScanEngine(t, types.LLMInspectionSpec{Mode: "block"})
		wantScanRefused(t, f, f.post(t, mcpToken("codex-cli"), "linear", eachCall(2, "delete_issue", `{"body":"`+mcpScanMarker+`"}`)), 2)
	})
}

// TestMCPBroker_ContentScanFailure: a detector that fails (here a custom detector answering null)
// refuses the body under on_scanner_error=block and is recorded on the allow row otherwise.
func TestMCPBroker_ContentScanFailure(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "null")
	}))
	t.Cleanup(sidecar.Close)
	t.Run("on_scanner_error=block", func(t *testing.T) {
		f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
		f.p.scanner = mcpScanEngine(t, types.LLMInspectionSpec{Mode: "block", OnScannerError: "block", DetectorSidecarURL: sidecar.URL})
		if sc := wantScanRefused(t, f, f.post(t, mcpToken("codex-cli"), "linear", mcpCall(3, "create_issue")), 3); sc.SkipReason != "sidecar_error" {
			t.Fatalf("deny row scan %+v, want skip reason sidecar_error", sc)
		}
	})
	t.Run("on_scanner_error=pass", func(t *testing.T) {
		f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
		f.p.scanner = mcpScanEngine(t, types.LLMInspectionSpec{Mode: "block", DetectorSidecarURL: sidecar.URL})
		if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(4, "create_issue")); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
			t.Fatalf("status %d hits %d; want it sent", rec.Code, f.up.hitCount())
		}
		if d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP); d.Scan == nil || d.Scan.Action != "skip" || d.Scan.SkipReason != "sidecar_error" {
			t.Fatalf("allow row scan %+v, want the sidecar_error skip", d.Scan)
		}
	})
}

// TestMCPBroker_ContentScanAlertRecorded: in alert mode the body is sent unchanged and the allow row
// carries the findings; with inspection on the model routes only, the broker does not scan.
func TestMCPBroker_ContentScanAlertRecorded(t *testing.T) {
	body := mcpMarkedCall(6, "create_issue")
	t.Run("alert", func(t *testing.T) {
		f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
		f.p.scanner = mcpScanEngine(t, types.LLMInspectionSpec{Mode: "alert"})
		if rec := f.post(t, mcpToken("codex-cli"), "linear", body); rec.Code != http.StatusOK || f.up.hitCount() != 1 || string(f.up.body) != body {
			t.Fatalf("status %d hits %d body %q; want the body sent unchanged", rec.Code, f.up.hitCount(), f.up.body)
		}
		d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP)
		if d.Scan == nil || d.Scan.Action != "alert" || d.Scan.Channel != string(contentscan.ChannelMCP) || len(d.Scan.Findings) == 0 {
			t.Fatalf("allow row scan %+v, want an alert summary with the marker finding", d.Scan)
		}
	})
	t.Run("model routes only", func(t *testing.T) {
		f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
		modelOnly, err := contentscan.NewEngine(types.LLMInspectionSpec{Mode: "block", ClassifiedMarkers: []string{mcpScanMarker}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.p.scanner = modelOnly
		if rec := f.post(t, mcpToken("codex-cli"), "linear", body); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
			t.Fatalf("status %d hits %d; want it sent", rec.Code, f.up.hitCount())
		}
		if d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP); d.Scan != nil {
			t.Fatalf("allow row scan %+v, want none", d.Scan)
		}
	})
}
