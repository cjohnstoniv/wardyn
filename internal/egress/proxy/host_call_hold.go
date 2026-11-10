// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The hold arm of host rules. A request the host rules hold (types.EvalHostRules says hold: no set
// denies it and at least one holds it) waits until a person decides its host, method and path
// (types.HostCallScope). A yes lets every request with that host, method and path through for the rest
// of the run in this proxy, and a no refuses them all; the table lives in this proxy only, so a new proxy
// asks again. The answer never outranks the rules: each request is judged afresh, so a deny in any set
// still refuses it, and only a request the rules hold reads the table.
//
// It fails closed, as the kubectl door's hold does (kube_api_hold.go): a request the run-wide
// allowed_methods refuses never asks anyone, a raise the control plane refuses or cannot take is
// refused, and an answer the proxy cannot read is still pending, so the request is never sent on it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// hostCallWindow is how long, in seconds, a held request waits for a person.
	hostCallWindow = 90

	ruleSourceHostRuleApprovalWait    = "policy:host-rule:approval-wait"
	ruleSourceHostRuleApprovalDenied  = "policy:host-rule:approval-denied"
	ruleSourceHostRuleApprovalClosed  = "policy:host-rule:approval-closed"
	ruleSourceHostRuleApprovalRefused = "policy:host-rule:approval-refused"

	hostCallHeldPrefix   = "Wardyn did not send this request: "
	hostCallWaitingText  = hostCallHeldPrefix + "a host rule holds %s %s on %s until a person decides (approval %s). Send it again once it is approved."
	hostCallDeclinedText = hostCallHeldPrefix + "a person declined %s %s on %s, so it is refused for the rest of the run."
	hostCallClosedText   = hostCallHeldPrefix + "its approval closed without a decision. Send it again to ask again."
	hostCallNoAskText    = hostCallHeldPrefix + "Wardyn could not ask a person about it. Send it again to ask again."
	hostCallRefusedText  = hostCallHeldPrefix + "the control plane refused the request for a person's approval (%d)."
	hostCallMethodText   = hostCallHeldPrefix + "the run's allowed methods refuse %s, so no one is asked."
)

// hostCallSecond is what one second of the window lasts. A var only so a test of the window need not
// wait real seconds.
var hostCallSecond = time.Second

// hostCalls is this proxy's table of host rule holds, by call key: the approval asked and its answer.
// The zero value is ready.
type hostCalls struct {
	mu sync.Mutex
	m  map[string]hostCallEntry
}

type hostCallEntry struct {
	id    uuid.UUID
	state approvalState // apPending, apApproved or apDenied
}

func (c *hostCalls) get(key string) hostCallEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

// ask records id as the pending approval for key unless the table already holds one, and returns what
// the table holds: the first raise wins, and a decided answer is never overwritten by a pending one.
func (c *hostCalls) ask(key string, id uuid.UUID) hostCallEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && e.id != uuid.Nil {
		return e
	}
	if c.m == nil {
		c.m = map[string]hostCallEntry{}
	}
	e := hostCallEntry{id: id, state: apPending}
	c.m[key] = e
	return e
}

// settle records the answer to approval id for key; apNone forgets it (closed without a decision), so
// the next request asks again. An answer for an approval the table no longer names is dropped.
func (c *hostCalls) settle(key string, id uuid.UUID, state approvalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; !ok || e.id != id {
		return
	}
	if state == apNone {
		delete(c.m, key)
		return
	}
	c.m[key] = hostCallEntry{id: id, state: state}
}

// holdHostCall is hostRuleRefused's arm for a request the rules hold: true when a person approved its
// host, method and path (now or earlier in the run), so the request goes on to the rest of the lane.
// When it reports false it has answered.
func (p *Proxy) holdHostCall(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	method := strings.ToUpper(r.Method)
	if !p.evaluator.MethodAllowed(r.Method) {
		p.answerHostHold(w, r, host, port, egress.Deny, ruleSourceHostRule, uuid.Nil,
			"host rules: a held request whose method allowed_methods refuses", fmt.Sprintf(hostCallMethodText, method))
		return false
	}
	path, err := types.NormalizeRequestPath(requestEscapedPath(r))
	if err != nil { // EvalHostRules denies a path it cannot read; this is the belt
		p.answerHostHold(w, r, host, port, egress.Deny, ruleSourceHostRule, uuid.Nil, "host rules: the request path cannot be judged: "+err.Error(),
			hostCallHeldPrefix+"its path cannot be judged.")
		return false
	}
	scope := types.HostCallScope{Host: host, Method: method, Path: path, HoldSeconds: hostCallWindow}
	scope.CallSHA256 = scope.ComputeKey()
	return p.awaitHostCall(w, r, host, port, scope)
}

// awaitHostCall passes an approved call, refuses a declined one, or asks (or rejoins the request pending
// for it) and waits on the open request up to the window.
func (p *Proxy) awaitHostCall(w http.ResponseWriter, r *http.Request, host string, port int, scope types.HostCallScope) bool {
	key := scope.CallSHA256
	declined := func(id uuid.UUID, cause string) bool {
		p.answerHostHold(w, r, host, port, egress.Deny, ruleSourceHostRuleApprovalDenied, id, cause,
			fmt.Sprintf(hostCallDeclinedText, scope.Method, scope.Path, scope.Host))
		return false
	}
	refused := func(cause, text string) bool {
		p.answerHostHold(w, r, host, port, egress.Deny, ruleSourceHostRuleApprovalRefused, uuid.Nil, cause, text)
		return false
	}
	e := p.hostCalls.get(key)
	switch e.state {
	case apApproved:
		return true
	case apDenied:
		return declined(e.id, "a declined host call, refused for the run")
	}
	if p.approval == nil {
		return refused("this proxy has no way to ask", hostCallNoAskText)
	}
	ctx, cancel := context.WithTimeout(r.Context(), hostCallWindow*hostCallSecond)
	defer cancel()
	if e.id != uuid.Nil {
		// A retry rejoining the pending request: the console reads it as held for this window too.
		_, _ = p.raiseHostCall(ctx, scope, e.id)
	} else {
		got, err := p.raiseHostCall(ctx, scope, uuid.Nil)
		var refusal *raiseRefusedError
		switch {
		case errors.As(err, &refusal):
			return refused(fmt.Sprintf("the request for an approval was refused (%d)", refusal.status), fmt.Sprintf(hostCallRefusedText, refusal.status))
		case err != nil:
			return refused("the request for an approval could not be raised", hostCallNoAskText)
		}
		if e = p.hostCalls.ask(key, got); e.state == apApproved {
			return true
		} else if e.state == apDenied {
			return declined(e.id, "a declined host call, refused for the run")
		}
	}
	return p.pollHostCall(ctx, w, r, host, port, scope, e.id, declined)
}

// pollHostCall waits on approval id for the scope's key: true once a person approved it.
func (p *Proxy) pollHostCall(ctx context.Context, w http.ResponseWriter, r *http.Request, host string, port int, scope types.HostCallScope,
	id uuid.UUID, declined func(uuid.UUID, string) bool) bool {
	key := scope.CallSHA256
	tick := time.NewTicker(holdPollInterval)
	defer tick.Stop()
	for {
		// Polled at once as well as on each tick: a request decided while nobody waited is answered
		// without a delay. An answer that cannot be read is still pending.
		if ar, ok := p.approval.fetch(ctx, id); ok {
			switch ar.State {
			case types.ApprovalApproved:
				p.hostCalls.settle(key, id, apApproved)
				return true
			case types.ApprovalDenied:
				p.hostCalls.settle(key, id, apDenied)
				return declined(id, "declined by a person")
			case types.ApprovalExpired, types.ApprovalCancelled:
				p.hostCalls.settle(key, id, apNone)
				p.answerHostHold(w, r, host, port, egress.Deny, ruleSourceHostRuleApprovalClosed, id, "closed without a decision", hostCallClosedText)
				return false
			}
		}
		select {
		case <-ctx.Done():
			if r.Context().Err() != nil {
				return false // the client went away; the request stays pending for a retry
			}
			p.answerHostHold(w, r, host, port, egress.Pending, ruleSourceHostRuleApprovalWait, id, "waiting for a person",
				fmt.Sprintf(hostCallWaitingText, scope.Method, scope.Path, scope.Host, id))
			return false
		case <-tick.C:
		}
	}
}

// raiseHostCall posts the hold's tool_call approval, or rejoins the pending one when rejoin is set.
func (p *Proxy) raiseHostCall(ctx context.Context, scope types.HostCallScope, rejoin uuid.UUID) (uuid.UUID, error) {
	var rejoinID *uuid.UUID
	if rejoin != uuid.Nil {
		rejoinID = &rejoin
	}
	body, err := json.Marshal(struct {
		Kind           types.ApprovalKind  `json:"kind"`
		RequestedScope types.HostCallScope `json:"requested_scope"`
		RejoinID       *uuid.UUID          `json:"rejoin_id,omitempty"`
	}{types.ApprovalToolCall, scope, rejoinID})
	if err != nil {
		return uuid.Nil, err
	}
	return p.approval.raiseBytes(ctx, body)
}

// answerHostHold writes one held-request row and answers: a pending 403 the sandbox reads as "retry after
// approval" for a wait, and the egress deny for every other outcome. The text rides the detail header.
func (p *Proxy) answerHostHold(w http.ResponseWriter, r *http.Request, host string, port int, d egress.Decision, source string,
	id uuid.UUID, cause, detail string) {
	log := decisionLog(p.reqOf(r, host, port), d, source)
	if id != uuid.Nil {
		log.ApprovalID = &id
	}
	log.Cause = cause
	if p.sink != nil {
		p.sink.emit(log)
	}
	w.Header().Set(egressHeaderDetail, detail)
	if d == egress.Pending {
		setEgressRefusalHeadersWithReason(w, egressRefusalPending, host, source)
		writeApprovalPending(w, &log)
		return
	}
	p.writeEgressDenyLLM(w, p.shapeOfHost(host), host, port, &log)
}
