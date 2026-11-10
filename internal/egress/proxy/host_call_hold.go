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
//
// It is bounded as the MCP broker's holds are, by the same table (mcpCalls, admitKept): at most
// maxMCPCallsWaiting requests wait at once (a retry of the same call counts), and at most
// maxMCPCallKeys distinct calls are remembered. A request past either bound is refused at once
// (ruleSourceHostRuleApprovalBusy), never held.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	ruleSourceHostRuleApprovalBusy    = "policy:host-rule:approval-busy"
	// ruleSourceHostRuleApprovalApproved is the allow row of a held request an approval let through,
	// naming the approval; the lane's own row for the request follows it.
	ruleSourceHostRuleApprovalApproved = "policy:host-rule:approval-approved"

	hostCallHeldPrefix   = "Wardyn did not send this request: "
	hostCallWaitingText  = hostCallHeldPrefix + "a host rule holds %s %s on %s until a person decides (approval %s). Send it again once it is approved."
	hostCallDeclinedText = hostCallHeldPrefix + "a person declined %s %s on %s, so it is refused for the rest of the run."
	hostCallClosedText   = hostCallHeldPrefix + "its approval closed without a decision. Send it again to ask again."
	hostCallNoAskText    = hostCallHeldPrefix + "Wardyn could not ask a person about it. Send it again to ask again."
	hostCallRefusedText  = hostCallHeldPrefix + "the control plane refused the request for a person's approval (%d)."
	hostCallMethodText   = hostCallHeldPrefix + "the run's allowed methods refuse %s, so no one is asked."
	hostCallBusyText     = hostCallHeldPrefix + "too many requests are already waiting for a person in this run. Try again later."
	hostCallKeysText     = hostCallHeldPrefix + "this run has asked about too many different requests; no more are asked about."
)

// hostCallSecond is what one second of the window lasts. A var only so a test of the window need not
// wait real seconds.
var hostCallSecond = time.Second

// hostRuleRefusedNotHeld is hostRuleRefused for the plain lane's first look, before the policy decides
// the host: a request the rules hold is passed on, to be held in handlePlain only once evaluate allows
// it, so a host the policy refuses never asks anyone.
func (p *Proxy) hostRuleRefusedNotHeld(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if p.policy.namesRuleHost(host) && types.EvalHostRules(p.policy.hostRules, host, r.Method, requestEscapedPath(r)).Effect == types.HostRuleHold {
		return false
	}
	return p.hostRuleRefused(w, r, host, port)
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
	id, state, why := p.hostCalls.admitKept(key)
	switch {
	case state == apApproved: // kept for the run, and no waiting slot taken
		return p.releaseHostCall(r, host, port, id)
	case state == apDenied:
		return declined(id, "a declined host call, refused for the run")
	case why != "":
		text, cause := hostCallBusyText, "too many host calls are waiting for a person"
		if why == mcpCallKeysText {
			text, cause = hostCallKeysText, "too many distinct host calls were asked about"
		}
		p.answerHostHold(w, r, host, port, egress.Deny, ruleSourceHostRuleApprovalBusy, uuid.Nil, cause, text)
		return false
	}
	defer p.hostCalls.leave(key)
	if p.approval == nil {
		return refused("this proxy has no way to ask", hostCallNoAskText)
	}
	ctx, cancel := context.WithTimeout(r.Context(), hostCallWindow*hostCallSecond)
	defer cancel()
	if id != uuid.Nil {
		// A retry rejoining the pending request: the console reads it as held for this window too.
		_, _ = p.raiseHostCall(ctx, scope, id)
	} else {
		got, err := p.raiseHostCall(ctx, scope, uuid.Nil)
		var refusal *raiseRefusedError
		switch {
		case errors.As(err, &refusal):
			return refused(fmt.Sprintf("the request for an approval was refused (%d)", refusal.status), fmt.Sprintf(hostCallRefusedText, refusal.status))
		case err != nil:
			return refused("the request for an approval could not be raised", hostCallNoAskText)
		}
		if id = p.hostCalls.install(key, got); id == uuid.Nil {
			return refused("the request for an approval named one this proxy cannot wait on", hostCallNoAskText)
		}
	}
	return p.pollHostCall(ctx, w, r, host, port, scope, id, declined)
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
				p.hostCalls.update(key, id, apApproved)
				return p.releaseHostCall(r, host, port, id)
			case types.ApprovalDenied:
				p.hostCalls.update(key, id, apDenied)
				return declined(id, "declined by a person")
			case types.ApprovalExpired, types.ApprovalCancelled:
				p.hostCalls.update(key, id, apNone)
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

// releaseHostCall writes the allow row of a held request approval id lets through, and reports true.
func (p *Proxy) releaseHostCall(r *http.Request, host string, port int, id uuid.UUID) bool {
	if p.sink != nil {
		log := decisionLog(p.reqOf(r, host, port), egress.Allow, ruleSourceHostRuleApprovalApproved)
		log.ApprovalID = &id
		p.sink.emit(log)
	}
	return true
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
