// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// mcpScanBlockedMessage is the JSON-RPC error a body the run's content inspection refuses is
// answered with, the skip reason appended when there is one.
const mcpScanBlockedMessage = "Blocked by this run's content inspection"

// scanBody runs the run's content inspection over the body forward is about to send, when the
// policy extends inspection past the model routes (inspect_forward_egress). It runs before the
// server's credential is resolved, on the exact bytes sent (an approved call's rebuilt body
// included), and decides as every other door does (ShouldBlock): a qualifying finding refuses in
// block mode, and a body it could not fully inspect refuses only under on_scanner_error=block. ok
// is false when it refused the body and answered; scan is the content-free summary the request's
// row carries, nil when there is nothing to report.
func (x *mcpExchange) scanBody(w http.ResponseWriter, body []byte) (scan *egress.ScanSummary, ok bool) {
	eng := x.p.scanner
	if body == nil || !eng.InspectForwardEgress() {
		return nil, true
	}
	// The scan slot bounds extraction's memory, as on the forward path; a body that waits past it
	// is not inspected, so it is not sent.
	free, err := x.p.scan.takeSlot(x.r.Context())
	if err != nil {
		x.deny(w, ruleSourceMCPRequest, "the MCP request body could not be inspected in time", http.StatusBadGateway)
		return nil, false
	}
	res, _, serr := eng.ScanRequestContext(x.r.Context(), contentscan.ChannelMCP, body)
	free()
	if !eng.ShouldBlock(res) {
		if len(res.Findings) > 0 || res.Skipped || serr != nil {
			scan = scanSummaryFrom(res, serr, eng, "", contentscan.ChannelMCP)
		}
		return scan, true
	}
	if x.p.sink != nil {
		d := decisionLog(x.req, egress.Deny, ruleSourceLLMBlocked)
		d.Scan = scanSummaryFrom(res, serr, eng, "block", contentscan.ChannelMCP)
		x.p.sink.emit(d)
	}
	msg := mcpScanBlockedMessage
	if res.SkipReason != "" {
		msg += " (" + res.SkipReason + ")"
	}
	// The body was read as JSON-RPC before it got here (refuseBlockedCalls, or buildMCPCall for
	// an approved call), so each request in it is answered in-protocol, as a tool-rule refusal is.
	msgs, batch, _ := parseMCPMessages(body)
	answerMCPRefused(w, msgs, batch, msg)
	return nil, false
}
