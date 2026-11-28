// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The pending-host half of the first-use approval client: the clock seam its timing reads, the one budget a
// request spends waiting, and the host's single-flight status check. approvals.go holds the state machine.

// approvalClock is the one seam the pending-host timing reads time through.
type approvalClock interface {
	Now() time.Time
	// AfterFunc runs f in its own goroutine once d has passed, and returns the call that stops it.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// pendingBudget is the ONE approvalCallTimeout a request gets for everything it waits on while its host is
// pending: the floor wait and the status check together. Its clock is the approvals client's.
type pendingBudget struct {
	clock    approvalClock
	ctx      context.Context
	cancel   context.CancelFunc
	deadline time.Time
	stopTick func() bool
}

func (b *pendingBudget) get(parent context.Context) context.Context {
	if b.ctx == nil {
		b.ctx, b.cancel = context.WithCancel(parent)
		b.deadline = b.clock.Now().Add(approvalCallTimeout)
		b.stopTick = b.clock.AfterFunc(approvalCallTimeout, b.cancel)
	}
	return b.ctx
}

// ended reports that the request's budget was started and has since run out or been cancelled. A request that
// has not waited yet has no budget and has not ended.
func (b *pendingBudget) ended() bool {
	return b.ctx != nil && b.ctx.Err() != nil
}

// within bounds a raise by what is left of the request's budget when it has one, so a request that waited on a
// check and then re-raises (a sibling spent the once grant) never takes a second approvalCallTimeout. A spent
// budget fails the raise at once, which rolls the host back to none and answers pending, as any failed raise does.
func (b *pendingBudget) within(parent context.Context) (context.Context, context.CancelFunc) {
	left := approvalCallTimeout
	if b.ctx != nil {
		left = b.deadline.Sub(b.clock.Now())
	}
	ctx, cancel := context.WithCancel(parent)
	if left <= 0 {
		cancel()
		return ctx, cancel
	}
	stop := b.clock.AfterFunc(left, cancel)
	return ctx, func() { stop(); cancel() }
}

func (b *pendingBudget) stop() {
	if b.cancel != nil {
		b.stopTick()
		b.cancel()
	}
}

// pendingPoll is a pending host's one in-flight status check. Its result fields are written under a.mu before
// done closes, and read only after.
type pendingPoll struct {
	done chan struct{}
	// decided is false when the check saw the approval still pending or failed: whoever waited on it is
	// answered pending.
	decided bool
	// applied is true when the decision was written to the host's entry. False for a decided check whose
	// approval id had been replaced meanwhile: its result is dropped.
	applied bool
	state   approvalState
}

// claimPendingPoll, with a.mu held, picks what a request on a pending host with a real id does next. It joins
// the host's in-flight check if there is one; else, when pendingPollFloor has passed since the last check, it
// starts one; else it returns the floor still to wait out. A request never gets the cached pending state just
// because a floor or another check is active.
func (a *approvalClient) claimPendingPoll(ctx context.Context, host string, st *hostApproval, id uuid.UUID) (*pendingPoll, time.Duration) {
	if st.pending != nil {
		return st.pending, 0
	}
	if left := pendingPollFloor - a.clock.Now().Sub(st.lastPoll); left > 0 {
		return nil, left
	}
	p := &pendingPoll{done: make(chan struct{})}
	st.pending = p
	st.lastPoll = a.clock.Now()
	// The check outlives a request that stops waiting, so the requests that joined it still get its answer;
	// it is bounded by approvalCallTimeout on its own.
	pollCtx, cancelPoll := context.WithCancel(context.WithoutCancel(ctx))
	stopTick := a.clock.AfterFunc(approvalCallTimeout, cancelPoll)
	go func() {
		defer func() { stopTick(); cancelPoll() }()
		decided, newState, polledScope, polledExpiry := a.poll(pollCtx, id)
		a.mu.Lock()
		defer a.mu.Unlock()
		defer close(p.done)
		p.decided, p.state = decided, newState
		e := a.hosts[host]
		if e.pending == p {
			e.pending = nil
		}
		// STALE POLLER: an approval replaced while this ran must not have its result written onto the new
		// one (see the approved branch of resolveOnce).
		if !decided || e.approvalID != id {
			return
		}
		p.applied = true
		if newState == apExpired {
			e.forget()
			return
		}
		e.state, e.scope, e.expiresAt = newState, polledScope, polledExpiry
		e.verifiedAt = time.Now()
	}()
	return p, 0
}

// waitPending spends the request's one budget on the floor wait, or on a check joined or started above, and
// answers it. Out of budget, or cancelled: pending, with no state change. A check that saw pending answers
// pending, and the next retry starts a fresh one. A decided one is not answered from here: the request re-reads
// the entry (again), where it claims or consumes the grant under the lock.
func (a *approvalClient) waitPending(budget context.Context, host string, p *pendingPoll, floor time.Duration, id uuid.UUID) resolveResult {
	pending := resolveResult{State: apPending, ApprovalID: id}
	// An ended budget wins over whatever finished beside it: a request that ran out of budget or was cancelled
	// answers pending and touches nothing, even if the floor or the check completed in the same instant.
	if p == nil {
		fired := make(chan struct{})
		stop := a.clock.AfterFunc(floor, func() { close(fired) })
		defer stop()
		select {
		case <-budget.Done():
			return pending
		case <-fired:
		}
		if budget.Err() != nil {
			return pending
		}
		return resolveResult{again: true}
	}
	select {
	case <-budget.Done():
		return pending
	case <-p.done:
	}
	if budget.Err() != nil {
		return pending
	}
	switch {
	case !p.decided:
		return pending
	case !p.applied:
		a.mu.Lock()
		defer a.mu.Unlock()
		return resolveResult{State: apPending, ApprovalID: a.hosts[host].approvalID}
	case p.state == apExpired:
		return resolveResult{State: apExpired, ApprovalID: id}
	}
	return resolveResult{again: true}
}
