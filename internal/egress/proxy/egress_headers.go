// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Egress-refusal header values. Both a deny and an approval-pending are a
// bare 403 whose body a CONNECT client discards, so the reason rides
// response HEADERS instead: the sandbox can tell "retry after approval" from
// "permanently blocked" and stop routing around a wait that just needs a human.
const (
	egressRefusalDenied  = "denied"
	egressRefusalPending = "approval-pending"
)

// egressHeaderStatus / egressHeaderHost / egressHeaderReason name those headers.
//
// The REASON header exists because "denied" alone is ambiguous: NINE
// distinct outcomes collapse into it (private-ip, resolve-failed,
// policy:denied, default-deny, method, approval:denied, evaluator-error,
// dial-failed...) that call for completely different developer actions. The
// value is the same static reason string the decision log already records,
// so this discloses nothing the audit trail does not.
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

// decisionReason extracts the decision log's RuleSource for the
// refusal-reason header, tolerating a nil log. RuleSource is the SAME static
// string the decision log records, never attacker-influenced text.
func decisionReason(log *egress.DecisionLog) string {
	if log == nil {
		return ""
	}
	return log.RuleSource
}
