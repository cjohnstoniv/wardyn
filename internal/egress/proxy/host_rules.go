// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Host rules (types.HostRuleSet) are judged per request, so a rule host is never an opaque tunnel:
//   - a CONNECT to one is refused in evaluate (ruleSourceHostRuleNeedsInspection) unless this proxy can
//     open it, and handleConnect then always terminates it (mitmConnect);
//   - each request inside, and each plain-HTTP request, is judged by types.EvalHostRules (ruleSourceHostRule);
//   - each forward a brokered route or API door sends is judged where it is sent (outboundHostRuleRefused).
//
// A via_org route is the exception, refused at load (viaOrgHosts).
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
// rules hold goes on: handlePlain holds it only once the policy allows the host (plainHostRuleHeld).
func (p *Proxy) servePlainJudged(w http.ResponseWriter, r *http.Request) {
	if r.URL != nil && r.URL.Host != "" {
		if host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme)); p.hostRuleRefusedNotHeld(w, r, host, port) {
			return
		}
	}
	p.servePlain(w, r)
}

// hostRuleUninspected says why a CONNECT to a rule host cannot be opened here, or "" when it can (or the
// host is no rule host).
func (p *Proxy) hostRuleUninspected(ctx context.Context, host string, port int) string {
	if !p.policy.namesRuleHost(host) {
		return ""
	}
	switch {
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

// hostRuleRefused judges one request to host (inside a terminated tunnel or on the plain lane), writes
// the refusal when the rules do not allow it, and reports whether it did. A request the rules hold waits
// for a person's answer to its host, method and path (host_call_hold.go); the request it returns carries
// an approved hold's admission to the outbound gate, so the lane forwards that one.
func (p *Proxy) hostRuleRefused(w http.ResponseWriter, r *http.Request, host string, port int) (*http.Request, bool) {
	if !p.policy.namesRuleHost(host) {
		return r, false
	}
	d := types.EvalHostRules(p.policy.hostRules, host, r.Method, requestEscapedPath(r))
	switch d.Effect {
	case types.HostRuleAllow:
		return r, false
	case types.HostRuleHold:
		r, ok := p.holdHostCall(w, r, host, port)
		return r, !ok
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceHostRule)
	log.Cause = hostRuleCause(d)
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

// errHostRuleRefused is a forward the host rules refuse (outboundHostRuleRefused).
var errHostRuleRefused = errors.New("wardyn: refused by host rules")

// outboundHostRuleRefused judges a request the proxy is about to send upstream, whichever route built it.
// The brokered routes (model, git, git credential, MCP) and the API doors re-originate outside the CONNECT
// and plain lanes, so this is where their requests are judged, on the method and path the host receives.
// A refusal writes the policy:host-rule row and returns errHostRuleRefused. A forward the rules hold is
// held as the lanes hold one (outboundHostCall); the request returned carries an approval's admission.
func (p *Proxy) outboundHostRuleRefused(req *http.Request) (*http.Request, error) {
	if req.URL == nil {
		return req, nil
	}
	host, port := splitHostPort(req.URL.Host, defaultPortForScheme(req.URL.Scheme))
	if !p.policy.namesRuleHost(host) {
		return req, nil
	}
	d := types.EvalHostRules(p.policy.hostRules, host, req.Method, req.URL.EscapedPath())
	switch d.Effect {
	case types.HostRuleAllow:
		return req, nil
	case types.HostRuleHold:
		return p.outboundHostCall(req, host, port)
	}
	log := decisionLog(p.reqOf(req, host, port), egress.Deny, ruleSourceHostRule)
	log.Cause = hostRuleCause(d)
	if p.sink != nil {
		p.sink.emit(log)
	}
	return req, errHostRuleRefused
}

// hostRuleTransportProxy is the egress transport's Proxy hook. The transport calls it for every request,
// a reused connection's included, so a route that sends on the transport itself (the kube door's clone of
// it) is judged as roundTripUpstream's callers are. It names no proxy.
func (p *Proxy) hostRuleTransportProxy(req *http.Request) (*url.URL, error) {
	_, err := p.outboundHostRuleRefused(req)
	return nil, err
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
// no rules on a VM run, and no rule on a host a via_org route serves (viaOrgHosts).
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
	viaOrg := c.viaOrgHosts()
	for _, s := range sets {
		for _, r := range s.Rules {
			if viaOrg[r.Host] {
				return fmt.Errorf("config: policy.host_rules: %s is reached through a via_org route, which host rules do not judge", r.Host)
			}
		}
	}
	return nil
}

// viaOrgHosts is every host a via_org route serves. Every other route's forward is judged where it is sent
// (outboundHostRuleRefused), but a via_org request goes to the organisation, which reaches the host itself
// (forwardViaOrg), and its git form names no host (viaOrgRouteFor), so a rule on such a host is refused.
func (c *Config) viaOrgHosts() map[string]bool {
	out := map[string]bool{}
	for _, r := range c.ViaOrgRoutes {
		out[hostKey(r.Host)] = true
		if slices.Contains(slices.Collect(maps.Values(c.GitGrants)), r.GrantID) {
			out["github.com"] = true
		}
	}
	return out
}
