// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Host rules (types.HostRuleSet) are judged per request, so a rule host is never an opaque tunnel:
//   - a CONNECT to one is refused in evaluate (ruleSourceHostRuleNeedsInspection) unless this proxy can
//     open it, and handleConnect then always terminates it (mitmConnect);
//   - each request inside, and each plain-HTTP request, is judged by types.EvalHostRules (ruleSourceHostRule).
//
// A brokered route re-originates to its host without either lane, so validateHostRules refuses a config
// whose rules name a host a broker serves.
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

// servePlainJudged is servePlain after host rules judge the request by its escaped path.
func (p *Proxy) servePlainJudged(w http.ResponseWriter, r *http.Request) {
	if r.URL != nil && r.URL.Host != "" {
		if host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme)); p.hostRuleRefused(w, r, host, port) {
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
// the refusal when the rules do not allow it, and reports whether it did. hold is refused until holds land.
func (p *Proxy) hostRuleRefused(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.policy.namesRuleHost(host) {
		return false
	}
	d := types.EvalHostRules(p.policy.hostRules, host, r.Method, requestEscapedPath(r))
	if d.Effect == types.HostRuleAllow {
		return false
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceHostRule)
	log.Cause = hostRuleCause(d)
	if p.sink != nil {
		p.sink.emit(log)
	}
	p.writeEgressDenyLLM(w, p.shapeOfHost(host), host, port, &log)
	return true
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
// A refusal writes the policy:host-rule row and returns errHostRuleRefused.
func (p *Proxy) outboundHostRuleRefused(req *http.Request) error {
	if req.URL == nil {
		return nil
	}
	host, port := splitHostPort(req.URL.Host, defaultPortForScheme(req.URL.Scheme))
	if !p.policy.namesRuleHost(host) {
		return nil
	}
	d := types.EvalHostRules(p.policy.hostRules, host, req.Method, req.URL.EscapedPath())
	if d.Effect == types.HostRuleAllow {
		return nil
	}
	log := decisionLog(p.reqOf(req, host, port), egress.Deny, ruleSourceHostRule)
	log.Cause = hostRuleCause(d)
	if p.sink != nil {
		p.sink.emit(log)
	}
	return errHostRuleRefused
}

// hostRuleTransportProxy is the egress transport's Proxy hook. The transport calls it for every request,
// a reused connection's included, so a route that sends on the transport itself (the kube door's clone of
// it) is judged as roundTripUpstream's callers are. It names no proxy.
func (p *Proxy) hostRuleTransportProxy(req *http.Request) (*url.URL, error) {
	return nil, p.outboundHostRuleRefused(req)
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
	case d.Effect == types.HostRuleHold:
		return fmt.Sprintf("host rules: rule set %d, rule %d holds this request, and holds are not available yet", d.Set, d.Rule)
	}
	return fmt.Sprintf("host rules: rule set %d, rule %d denies this request", d.Set, d.Rule)
}

// namesRuleHost reports whether any of the policy's host rule sets names host.
func (p *Policy) namesRuleHost(host string) bool {
	return p != nil && len(p.hostRules) > 0 && types.HostRulesName(p.hostRules, host)
}

// validateHostRules is the config's check of policy.host_rules: the shape (types.ValidateHostRuleSets),
// no rules on a VM run, and no rule host that a brokered route re-originates to, since a broker forwards
// outside the lanes the rules are judged on.
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
	brokered := c.brokeredHosts()
	for _, s := range sets {
		for _, r := range s.Rules {
			if lane, ok := brokered[r.Host]; ok {
				return fmt.Errorf("config: policy.host_rules: %s is reached through the %s, which host rules do not judge", r.Host, lane)
			}
		}
	}
	return nil
}

// brokeredHosts is every host a brokered local route re-originates to, with the route's name.
func (c *Config) brokeredHosts() map[string]string {
	out := map[string]string{anthropicHost: "brokered model route", openaiHost: "brokered model route"}
	addURL := func(raw, lane string) {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			out[hostKey(u.Hostname())] = lane
		}
	}
	for vendor, up := range c.LLMUpstreams {
		out[hostKey(vendor)] = "brokered model route"
		addURL(up, "brokered model route")
	}
	if len(c.GitGrants) > 0 {
		out["github.com"] = "git broker"
	}
	for h := range c.PATGrants {
		out[hostKey(h)] = "git credential broker"
	}
	for _, s := range c.MCPServers {
		addURL(s.URL, "MCP broker")
	}
	for k := range c.LocalModels {
		h, _ := splitHostPort(k, 0)
		out[h] = "brokered model route"
	}
	for _, r := range c.GatewayIdentity {
		out[hostKey(r.Host)] = "brokered model route"
	}
	for _, r := range c.ViaOrgRoutes {
		out[hostKey(r.Host)] = "via_org route"
	}
	return out
}
