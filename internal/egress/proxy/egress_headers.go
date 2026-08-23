// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "net/http"

// Egress-refusal header values. Both a deny and an approval-pending are a bare
// 403 whose body a CONNECT client discards, so the reason rides response HEADERS
// (visible to `curl -sD-`) instead: the sandbox can then tell "retry after
// approval" (pending) from "permanently blocked" (denied) and stop routing
// around a wait that just needs a human. See deploy/images/common/attach-bashrc.
const (
	egressRefusalDenied  = "denied"
	egressRefusalPending = "approval-pending"
)

// egressHeaderStatus / egressHeaderHost name those headers.
const (
	egressHeaderStatus = "X-Wardyn-Egress"
	egressHeaderHost   = "X-Wardyn-Host"
)

// setEgressRefusalHeaders annotates a deny/pending 403 with the refusal reason
// and the host it applies to. Set BEFORE http.Error / writeApprovalPending,
// which flush the header block on WriteHeader.
func setEgressRefusalHeaders(w http.ResponseWriter, status, host string) {
	w.Header().Set(egressHeaderStatus, status)
	if host != "" {
		w.Header().Set(egressHeaderHost, host)
	}
}
