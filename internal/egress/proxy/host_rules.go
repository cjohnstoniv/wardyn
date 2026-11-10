// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Host rules (types.HostRuleSet) are judged per request, so a rule host is never an opaque tunnel:
//   - a CONNECT to one is refused in evaluate (ruleSourceHostRuleNeedsInspection) unless this proxy can
//     open it, and handleConnect then always terminates it (mitmConnect);
//   - each request inside, and each plain-HTTP request, is judged by types.EvalHostRules (ruleSourceHostRule);
//   - each forward a brokered route or API door sends is judged where it is sent (outboundHostRuleRefused).
//
// A via_org engine judges a forwarded request the same way (ViaOrgEngine.ServeInjected, ServeBroker).
const (
	ruleSourceHostRule                = "policy:host-rule"
	ruleSourceHostRuleNeedsInspection = "policy:host-rule-needs-inspection"
)

// hostRuleConnectDeny is evaluate's CONNECT half: the deny row for a CONNECT to a rule host this proxy
// cannot open, or nil. It runs before any first-use approval: no approval makes an unopened tunnel judgeable.
func (p *Proxy) hostRuleConnectDeny(ctx context.Context, req egress.Request) *egress.DecisionLog {
	if req.Method != http.MethodConnect {
		return nil
	}
	cause := p.hostRuleUninspected(ctx, req.Host, req.Port)
	if cause == "" {
		return nil
	}
	log := decisionLog(req, egress.Deny, ruleSourceHostRuleNeedsInspection)
	log.Cause = cause
	return &log
}

// servePlainJudged is servePlain after host rules judge the request by its escaped path. A request the
// rules hold goes on: handlePlain holds it only once the policy allows the host (hostRuleRefusedNotHeld).
func (p *Proxy) servePlainJudged(w http.ResponseWriter, r *http.Request) {
	if r.URL != nil && r.URL.Host != "" {
		if host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme)); p.hostRuleRefusedNotHeld(w, r, host, port) {
			return
		}
	}
	p.servePlain(w, r)
}

// engineHeldRequest marks the destination check a via_org engine runs for a request it already holds in
// the clear (ViaOrgEngine.ServeInjected): there is no tunnel to open, so the check needs no interception
// certificate. Only that call sets it, and only an engine's proxy reads it; a sandbox's CONNECT never has it.
type engineHeldRequest struct{}

// hostRuleUninspected says why a CONNECT to a rule host cannot be opened here, or "" when it can (or the
// host is no rule host).
func (p *Proxy) hostRuleUninspected(ctx context.Context, host string, port int) string {
	if !p.policy.namesRuleHost(host) {
		return ""
	}
	switch {
	case p.engine && ctx.Value(engineHeldRequest{}) != nil:
		return "" // the request itself is judged next, in serveMITMRequest (ServeInjected)
	case p.vm:
		return "host rules are not judged on a VM run's guest flows"
	case onSigninLane(ctx):
		return "the sign-in lane does not open connections, so a rule host is not reached on it"
	case p.ca == nil:
		return "this run has no TLS interception certificate, so its requests to a rule host cannot be judged"
	case !p.mitmPortAllowed(host, port):
		return fmt.Sprintf("port %d is not a port this proxy opens for %s", port, host)
	}
	return ""
}

// hostRuleVerdict is the one judgement both gates (hostRuleRefused, outboundHostRuleRefused) make of a
// request to host with the escaped path rawPath: "" when the host is no rule host or its rules allow the
// request, otherwise the refusal's cause. held reports a request the rules hold for a person
// (host_call_hold.go): no set denies it, it carries no override, and its cause is "".
func (p *Proxy) hostRuleVerdict(r *http.Request, host, rawPath string) (cause string, held bool) {
	if !p.policy.namesRuleHost(host) {
		return "", false
	}
	if cause := routeOverrideCause(r); cause != "" {
		return cause, false
	}
	switch d := types.EvalHostRules(p.policy.hostRules, host, r.Method, rawPath); d.Effect {
	case types.HostRuleAllow:
		return "", false
	case types.HostRuleHold:
		return "", true
	default:
		return hostRuleCause(d), false
	}
}

// pathOverrideHeaders are headers a server or framework may route by in place of the request line's path.
var pathOverrideHeaders = []string{"X-Original-URL", "X-Rewrite-URL", "X-Override-URL"}

// routeOverrideCause says why a request carries a header that could have its host run another method or
// path than the one the rules judged, or "": a method override naming another method (patAPIMethodMismatch,
// the forge API door's check), or any path override. Such a request is refused, never stripped: the rule
// says what the host may be asked, and the request asks something else.
func routeOverrideCause(r *http.Request) string {
	if patAPIMethodMismatch(r) {
		return "host rules: the request carries a method override that names another method than the request line"
	}
	for _, k := range pathOverrideHeaders {
		if len(r.Header.Values(k)) > 0 {
			return "host rules: the request carries " + k + ", which a host may serve in place of the path judged"
		}
	}
	return ""
}

// hostRuleRefused judges one request to host (inside a terminated tunnel or on the plain lane), writes
// the refusal when the rules do not allow it, and reports whether it did. A request the rules hold waits
// for a person's answer to its host, method and path (host_call_hold.go); the request it returns carries
// an approved hold's admission to the outbound gate, so the lane forwards that one.
func (p *Proxy) hostRuleRefused(w http.ResponseWriter, r *http.Request, host string, port int) (*http.Request, bool) {
	cause, held := p.hostRuleVerdict(r, host, requestEscapedPath(r))
	if held {
		r, ok := p.holdHostCall(w, r, host, port)
		return r, !ok
	}
	if cause == "" {
		return r, false
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceHostRule)
	log.Cause = cause
	if p.sink != nil {
		p.sink.emit(log)
	}
	p.writeEgressDenyLLM(w, p.shapeOfHost(host), host, port, &log)
	return r, true
}

// innerMethodRefused is allowed_methods on a request inside a terminated tunnel (serveAdmittedMITMRequest):
// it writes the policy:method refusal and reports whether it did.
func (p *Proxy) innerMethodRefused(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if p.evaluator.MethodAllowed(r.Method) {
		return false
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Deny, "policy:method")
	if p.sink != nil {
		p.sink.emit(log)
	}
	// memoed=false: built right here (policy:method on an inner request), never from the private-IP memo.
	p.writeEgressDenyLLM(w, p.shapeOfHost(host), host, port, &log)
	return true
}

// proxyOwnSend marks a request the proxy sends for itself, not on the sandbox's behalf (withProxyOwnSend):
// the GitHub token hand-back, the push comparison's forge reads, the AWS portal's role credentials. The
// run's host rules govern what the sandbox may ask a host, so the outbound gate passes these. Only the proxy
// sets it, in the context; no header or sandbox input can.
type proxyOwnSend struct{}

func withProxyOwnSend(ctx context.Context) context.Context {
	return context.WithValue(ctx, proxyOwnSend{}, true)
}

// errHostRuleRefused is a forward the host rules refuse (outboundHostRuleRefused); the error returned is a
// *hostRuleRefusal, which carries the row it wrote so the answer can be the same 403 the other lanes give.
var errHostRuleRefused = errors.New("wardyn: refused by host rules")

// hold, when set, is a held forward's answer (host_call_hold.go): the pending 403 for a wait.
type hostRuleRefusal struct {
	log  egress.DecisionLog
	hold *hostHoldAnswer
}

func (e *hostRuleRefusal) Error() string        { return errHostRuleRefused.Error() }
func (e *hostRuleRefusal) Is(target error) bool { return target == errHostRuleRefused }

// answerHostRuleRefusal answers err when it is a host-rule refusal: the 403 deny, shaped for the route,
// with no further row (outboundHostRuleRefused wrote it). It reports whether it answered.
func (p *Proxy) answerHostRuleRefusal(w http.ResponseWriter, shape errShape, err error) bool {
	var hr *hostRuleRefusal
	if !errors.As(err, &hr) {
		return false
	}
	if hr.hold != nil {
		p.writeHostHold(w, shape, hr.log.Request.Host, hr.log.Request.Port, *hr.hold, &hr.log)
		return true
	}
	p.writeEgressDenyLLM(w, shape, hr.log.Request.Host, hr.log.Request.Port, &hr.log)
	return true
}

// outboundHostRuleRefused judges a request the proxy is about to send upstream, whichever route built it.
// The brokered routes (model, git, git credential, MCP) and the API doors re-originate outside the CONNECT
// and plain lanes, so this is where their requests are judged, on the method and path the host receives.
// A refusal writes the policy:host-rule row and returns errHostRuleRefused. A forward the rules hold is
// held as the lanes hold one (outboundHostCall); the request returned carries an approval's admission.
func (p *Proxy) outboundHostRuleRefused(req *http.Request) (*http.Request, error) {
	if req.URL == nil || req.Context().Value(proxyOwnSend{}) != nil {
		return req, nil
	}
	host, port := splitHostPort(req.URL.Host, defaultPortForScheme(req.URL.Scheme))
	cause, held := p.hostRuleVerdict(req, host, req.URL.EscapedPath())
	if held {
		return p.outboundHostCall(req, host, port)
	}
	if cause == "" {
		return req, nil
	}
	log := decisionLog(p.reqOf(req, host, port), egress.Deny, ruleSourceHostRule)
	log.Cause = cause
	if p.sink != nil {
		p.sink.emit(log)
	}
	return req, &hostRuleRefusal{log: log}
}

// requestEscapedPath is the path as the request carried it, escapes kept, so an encoded "/" is seen and
// refused rather than decoded into a segment boundary.
func requestEscapedPath(r *http.Request) string {
	if r.URL == nil {
		return "/"
	}
	return r.URL.EscapedPath()
}

// hostRuleCause names the set and rule that refused, for the decision row.
func hostRuleCause(d types.HostRuleDecision) string {
	switch {
	case d.Set < 0:
		return "host rules: the request path cannot be judged: " + d.Cause
	case d.Rule < 0:
		return "host rules: " + d.Cause
	}
	return fmt.Sprintf("host rules: rule set %d, rule %d denies this request", d.Set, d.Rule)
}

// namesRuleHost reports whether any of the policy's host rule sets names host.
func (p *Policy) namesRuleHost(host string) bool {
	return p != nil && len(p.hostRules) > 0 && types.HostRulesName(p.hostRules, host)
}

// validateHostRules is the config's check of policy.host_rules: the shape (types.ValidateHostRuleSets),
// and no rules on a VM run.
func (c *Config) validateHostRules() error {
	sets := c.Policy.HostRules
	if len(sets) == 0 {
		return nil
	}
	if err := types.ValidateHostRuleSets(sets); err != nil {
		return fmt.Errorf("config: policy.%w", err)
	}
	if c.VM {
		return fmt.Errorf("config: policy.host_rules: host rules are not judged on a VM run's guest flows")
	}
	return nil
}
