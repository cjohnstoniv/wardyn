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
// It fails closed, as the kubectl door's hold does (kube_api_hold.go): a raise the control plane refuses
// or cannot take is refused, and an answer the proxy cannot read is still pending, so the request is never
// sent on it. On the lanes the run-wide allowed_methods covers, a request it refuses never asks anyone
// (holdHostCall). The outbound gate does not check it (outboundHostCall): the git routes ignore it.
//
// It is bounded as the MCP broker's holds are, by the same table (mcpCalls, admitKept): at most
// maxMCPCallsWaiting requests wait at once (a retry of the same call counts), and at most
// maxMCPCallKeys distinct calls are remembered at once; a key whose approval closed undecided is freed
// (mcpCalls.tidy). A request past either bound is refused at once
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
// the host (a held request to a host the policy refuses never asks a person).
func (p *Proxy) hostRuleRefusedNotHeld(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if _, held := p.hostRuleVerdict(r, host, requestEscapedPath(r)); held {
		return false
	}
	_, refused := p.hostRuleRefused(w, r, host, port)
	return refused
}

// hostCallAdmittedKey is the request context key under which a lane or the outbound gate records the
// held request it judged and a person approved (its HostCallKey), so the outbound gate does not judge the
// same request again. Only the proxy sets a context value; nothing the sandbox sends becomes one.
type hostCallAdmittedKey struct{}

// withHostCallAdmitted is r carrying the admitted key of scope.
func withHostCallAdmitted(r *http.Request, scope types.HostCallScope) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), hostCallAdmittedKey{}, scope.CallSHA256))
}

// hostCallAdmitted reports whether ctx carries the admitted key of scope.
func hostCallAdmitted(ctx context.Context, scope types.HostCallScope) bool {
	k, _ := ctx.Value(hostCallAdmittedKey{}).(string)
	return k != "" && k == scope.CallSHA256
}

// hostHoldAnswer is a held request's refusal: the row it writes and the text the sandbox reads.
type hostHoldAnswer struct {
	decision egress.Decision
	source   string
	id       uuid.UUID
	cause    string
	detail   string
}

// hostCallScopeOf is the held request's scope: the host, the method, and the path as the host receives it.
func hostCallScopeOf(host, method, escapedPath string) (types.HostCallScope, *hostHoldAnswer) {
	path, err := types.NormalizeRequestPath(escapedPath)
	if err != nil { // EvalHostRules denies a path it cannot read; this is the belt
		return types.HostCallScope{}, &hostHoldAnswer{egress.Deny, ruleSourceHostRule, uuid.Nil,
			"host rules: the request path cannot be judged: " + err.Error(), hostCallHeldPrefix + "its path cannot be judged."}
	}
	scope := types.HostCallScope{Host: host, Method: strings.ToUpper(method), Path: path, HoldSeconds: hostCallWindow}
	scope.CallSHA256 = scope.ComputeKey()
	return scope, nil
}

// holdHostCall is hostRuleRefused's arm for a request the rules hold. When a person approved its host,
// method and path (now or earlier in the run) it returns the request carrying that admission, and true,
// so the request goes on to the rest of the lane. When it reports false it has answered.
func (p *Proxy) holdHostCall(w http.ResponseWriter, r *http.Request, host string, port int) (*http.Request, bool) {
	if !p.evaluator.MethodAllowed(r.Method) {
		p.answerHostHold(w, r, host, port, hostHoldAnswer{egress.Deny, ruleSourceHostRule, uuid.Nil,
			"host rules: a held request whose method allowed_methods refuses", fmt.Sprintf(hostCallMethodText, strings.ToUpper(r.Method))})
		return r, false
	}
	scope, bad := hostCallScopeOf(host, r.Method, requestEscapedPath(r))
	if bad != nil {
		p.answerHostHold(w, r, host, port, *bad)
		return r, false
	}
	ok, refusal := p.awaitHostCall(r, host, port, scope)
	if !ok {
		if refusal != nil { // nil: the client went away, and there is nobody to answer
			p.answerHostHold(w, r, host, port, *refusal)
		}
		return r, false
	}
	return withHostCallAdmitted(r, scope), true
}

// outboundHostCall is the outbound gate's arm for a forward the rules hold, on the host, method and path
// the host receives. A forward a lane already admitted for that same host call goes on unasked; any other
// is held here as the lanes hold one. On approval it returns the forward carrying the admission, so the
// route sends that one and a second look at the same forward passes it without a second row.
func (p *Proxy) outboundHostCall(req *http.Request, host string, port int) (*http.Request, error) {
	scope, bad := hostCallScopeOf(host, req.Method, req.URL.EscapedPath())
	if bad != nil {
		return req, &hostRuleRefusal{log: p.emitHostHold(req, host, port, *bad), hold: bad}
	}
	if hostCallAdmitted(req.Context(), scope) {
		return req, nil
	}
	ok, refusal := p.awaitHostCall(req, host, port, scope)
	switch {
	case ok:
		return withHostCallAdmitted(req, scope), nil
	case refusal == nil: // the client went away: no row, and the answer reaches nobody
		gone := hostHoldAnswer{egress.Pending, ruleSourceHostRuleApprovalWait, uuid.Nil, "the client went away", hostCallHeldPrefix + "the client went away."}
		return req, &hostRuleRefusal{log: decisionLog(p.reqOf(req, host, port), egress.Pending, gone.source), hold: &gone}
	}
	return req, &hostRuleRefusal{log: p.emitHostHold(req, host, port, *refusal), hold: refusal}
}

// awaitHostCall passes an approved call, refuses a declined one, or asks (or rejoins the request pending
// for it) and waits on the open request up to the window. ok false with no refusal is a client gone.
func (p *Proxy) awaitHostCall(r *http.Request, host string, port int, scope types.HostCallScope) (bool, *hostHoldAnswer) {
	key := scope.CallSHA256
	declined := func(id uuid.UUID, cause string) *hostHoldAnswer {
		return &hostHoldAnswer{egress.Deny, ruleSourceHostRuleApprovalDenied, id, cause,
			fmt.Sprintf(hostCallDeclinedText, scope.Method, scope.Path, scope.Host)}
	}
	refused := func(cause, text string) *hostHoldAnswer {
		return &hostHoldAnswer{egress.Deny, ruleSourceHostRuleApprovalRefused, uuid.Nil, cause, text}
	}
	id, state, why := p.hostCalls.admitKept(key)
	switch {
	case state == apApproved: // kept for the run, and no waiting slot taken
		return p.releaseHostCall(r, host, port, id), nil
	case state == apDenied:
		return false, declined(id, "a declined host call, refused for the run")
	case why != "":
		text, cause := hostCallBusyText, "too many host calls are waiting for a person"
		if why == mcpCallKeysText {
			text, cause = hostCallKeysText, "too many distinct host calls were asked about"
		}
		return false, &hostHoldAnswer{egress.Deny, ruleSourceHostRuleApprovalBusy, uuid.Nil, cause, text}
	}
	defer p.hostCalls.leave(key)
	if p.approval == nil {
		return false, refused("this proxy has no way to ask", hostCallNoAskText)
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
			return false, refused(fmt.Sprintf("the request for an approval was refused (%d)", refusal.status), fmt.Sprintf(hostCallRefusedText, refusal.status))
		case err != nil:
			return false, refused("the request for an approval could not be raised", hostCallNoAskText)
		}
		if id = p.hostCalls.install(key, got); id == uuid.Nil {
			return false, refused("the request for an approval named one this proxy cannot wait on", hostCallNoAskText)
		}
	}
	return p.pollHostCall(ctx, r, host, port, scope, id, declined)
}

// pollHostCall waits on approval id for the scope's key: true once a person approved it.
func (p *Proxy) pollHostCall(ctx context.Context, r *http.Request, host string, port int, scope types.HostCallScope,
	id uuid.UUID, declined func(uuid.UUID, string) *hostHoldAnswer) (bool, *hostHoldAnswer) {
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
				return p.releaseHostCall(r, host, port, id), nil
			case types.ApprovalDenied:
				p.hostCalls.update(key, id, apDenied)
				return false, declined(id, "declined by a person")
			case types.ApprovalExpired, types.ApprovalCancelled:
				p.hostCalls.update(key, id, apNone)
				return false, &hostHoldAnswer{egress.Deny, ruleSourceHostRuleApprovalClosed, id, "closed without a decision", hostCallClosedText}
			}
		}
		select {
		case <-ctx.Done():
			if r.Context().Err() != nil {
				return false, nil // the client went away; the request stays pending for a retry
			}
			return false, &hostHoldAnswer{egress.Pending, ruleSourceHostRuleApprovalWait, id, "waiting for a person",
				fmt.Sprintf(hostCallWaitingText, scope.Method, scope.Path, scope.Host, id)}
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

// emitHostHold writes a held request's refusal row and returns it.
func (p *Proxy) emitHostHold(r *http.Request, host string, port int, a hostHoldAnswer) egress.DecisionLog {
	log := decisionLog(p.reqOf(r, host, port), a.decision, a.source)
	if a.id != uuid.Nil {
		log.ApprovalID = &a.id
	}
	log.Cause = a.cause
	if p.sink != nil {
		p.sink.emit(log)
	}
	return log
}

// answerHostHold writes one held-request row and answers (writeHostHold).
func (p *Proxy) answerHostHold(w http.ResponseWriter, r *http.Request, host string, port int, a hostHoldAnswer) {
	log := p.emitHostHold(r, host, port, a)
	p.writeHostHold(w, p.shapeOfHost(host), host, port, a, &log)
}

// writeHostHold answers a held request: a pending 403 the sandbox reads as "retry after approval" for a
// wait, and the egress deny for every other outcome. The text rides the detail header.
func (p *Proxy) writeHostHold(w http.ResponseWriter, shape errShape, host string, port int, a hostHoldAnswer, log *egress.DecisionLog) {
	w.Header().Set(egressHeaderDetail, a.detail)
	if a.decision == egress.Pending {
		setEgressRefusalHeadersWithReason(w, egressRefusalPending, host, a.source)
		writeApprovalPending(w, log)
		return
	}
	p.writeEgressDenyLLM(w, shape, host, port, log)
}
