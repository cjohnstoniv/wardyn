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
// consumeIfOnce left it. A request for host since then that raised or polled an approval puts that one in force;
// one whose raise failed leaves the entry as it was. One approval comes back at most maxOnceRefunds times. The
// entry it writes is a terminal once grant, which every path that serves a grant spends under a.mu
// (consumeIfOnce), so it goes to one caller at most. It reports whether it gave the grant back.
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

// nameGivenBackUse marks dl, the failure's row, with the approval seen's request went ahead on when that approval
// was one refundOnce gave back (seen.GivenBackUse, decided when the request took the grant): the use that spends it
// for good names it, without the not-spent note. It marks nothing for any other request and gives nothing back.
func nameGivenBackUse(dl, seen *egress.DecisionLog) {
	if seen != nil && seen.GivenBackUse && seen.ApprovalID != nil {
		dl.ApprovalID = seen.ApprovalID
	}
}

// refundOnceUse gives back the once grant that released seen, the use's allow, after that use got nothing back
// from the far side, and then marks dl, the failure's row: it names the approval and says it was not spent. At the
// dial it runs before the 502, so the sandbox's retry finds the grant. At a dead tunnel the sandbox's leg is already
// closed, so a retry that arrives first asks again. A run with no network gets none back: its retry cannot succeed
// either. A use of a grant already given back once is spent by its failure, and its row names the approval too
// (nameGivenBackUse).
func (p *Proxy) refundOnceUse(dl, seen *egress.DecisionLog, host string) {
	nameGivenBackUse(dl, seen)
	if p.approval == nil || p.noNetwork || seen == nil || seen.ApprovalID == nil {
		return
	}
	if p.approval.refundOnce(host, *seen.ApprovalID) {
		dl.ApprovalID = seen.ApprovalID
		dl.Cause += onceRefundNote
	}
}
