// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// onceRefundTTL is how long a once grant refundOnce gave back waits for the request it is for. Past it the grant
// lapses through consumeIfOnce's expiry and the next request asks again. onceRefundNote states it.
const onceRefundTTL = 60 * time.Second

// maxOnceRefunds is how many times refundOnce gives one approval back.
const maxOnceRefunds = 1

// onceRefundNote ends the cause of a failure whose once grant refundOnce gave back.
const onceRefundNote = "; the once approval was not spent: the next request to this host within 60 seconds uses it"

// refundOnce gives the once grant of approval id, spent on host, back to the next request for host: its caller saw
// the use that grant released end before the destination answered. It acts only on the entry exactly as
// consumeIfOnce left it. A request for host since then raised or polled an approval, and that one is in force. One
// approval comes back at most maxOnceRefunds times. The entry it writes is a terminal once grant, which every path
// that serves a grant spends under a.mu (consumeIfOnce), so it goes to one caller at most. It reports whether it
// gave the grant back.
func (a *approvalClient) refundOnce(host string, id uuid.UUID) bool {
	if id == uuid.Nil {
		return false
	}
	host = approvalHostKey(host)
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.hosts[host]
	if st == nil || st.state != apNone || st.approvalID != uuid.Nil || st.spent != id || st.refunds >= maxOnceRefunds {
		return false
	}
	*st = hostApproval{
		state:      apApproved,
		approvalID: id,
		scope:      types.ScopeOnce,
		expiresAt:  time.Now().Add(onceRefundTTL),
		spent:      id,
		refunds:    st.refunds + 1,
	}
	return true
}

// refundOnceUse gives back the once grant that released seen, the use's allow, after that use got nothing back
// from the far side, and then marks dl, the failure's row: it names the approval and says it was not spent. At the
// dial it runs before the 502, so the sandbox's retry finds the grant. At a dead tunnel the sandbox's leg is already
// closed, so a retry that arrives first asks again. A run with no network gets none back: its retry cannot succeed
// either.
func (p *Proxy) refundOnceUse(dl, seen *egress.DecisionLog, host string) {
	if p.approval == nil || p.noNetwork || seen == nil || seen.ApprovalID == nil {
		return
	}
	if p.approval.refundOnce(host, *seen.ApprovalID) {
		dl.ApprovalID = seen.ApprovalID
		dl.Cause += onceRefundNote
	}
}
