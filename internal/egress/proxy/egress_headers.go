// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Egress-refusal header values. Both a deny and an approval-pending are a bare
// 403 whose body a CONNECT client discards, so the reason rides response HEADERS
// (visible to `curl -sD-`) instead: the sandbox can then tell "retry after
// approval" (pending) from "permanently blocked" (denied) and stop routing
// around a wait that just needs a human. See deploy/images/common/attach-bashrc.
const (
	egressRefusalDenied  = "denied"
	egressRefusalPending = "approval-pending"
)

// egressHeaderStatus / egressHeaderHost / egressHeaderReason name those headers.
//
// The REASON header exists because "denied" alone is ambiguous in a way that
// costs a developer real time: NINE distinct outcomes collapse into it —
// builtin:private-ip, builtin:resolve-failed (the name never resolved, which is
// not the address-range guard), policy:denied (an explicit deny-list hit),
// policy:default-deny (simply not on the allowlist), policy:method,
// approval:denied (a human said no), policy:evaluator-error and
// builtin:dial-failed among them. Those call for completely different actions —
// ask the operator to allowlist a host, versus stop trying because a human
// already refused, versus fix a broken policy — and the sandbox could not tell
// them apart. The value is the same static reason string the decision log
// already records, so this discloses nothing the audit trail does not.
const (
	egressHeaderStatus = "X-Wardyn-Egress"
	egressHeaderHost   = "X-Wardyn-Host"
	egressHeaderReason = "X-Wardyn-Egress-Reason"
)

// setEgressRefusalHeaders annotates a deny/pending 403 with the refusal reason
// and the host it applies to. Set BEFORE http.Error / writeApprovalPending,
// which flush the header block on WriteHeader.
func setEgressRefusalHeaders(w http.ResponseWriter, status, host string) {
	setEgressRefusalHeadersWithReason(w, status, host, "")
}

// setEgressRefusalHeadersWithReason is setEgressRefusalHeaders plus the
// decision log's reason. Separate rather than a changed signature so the two
// callers that genuinely have no reason in scope stay honest about it.
func setEgressRefusalHeadersWithReason(w http.ResponseWriter, status, host, reason string) {
	if reason != "" {
		w.Header().Set(egressHeaderReason, reason)
	}
	w.Header().Set(egressHeaderStatus, status)
	if host != "" {
		w.Header().Set(egressHeaderHost, host)
	}
}

// decisionReason extracts the decision log's RuleSource for the refusal-reason
// header, tolerating a nil log.
//
// RuleSource is the SAME static string the decision log records — never
// attacker-influenced text — so surfacing it to the sandbox discloses nothing
// the audit trail does not already hold, and there is no user input to sanitise.
func decisionReason(log *egress.DecisionLog) string {
	if log == nil {
		return ""
	}
	return log.RuleSource
}
