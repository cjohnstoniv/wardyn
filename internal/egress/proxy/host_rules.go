// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
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

// hostRuleUninspected is evaluate's CONNECT half: why a CONNECT to a rule host cannot be opened here,
// or "" when it can (or the host is no rule host).
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
// for a person's answer to its host, method and path (host_call_hold.go).
func (p *Proxy) hostRuleRefused(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.policy.namesRuleHost(host) {
		return false
	}
	d := types.EvalHostRules(p.policy.hostRules, host, r.Method, requestEscapedPath(r))
	switch d.Effect {
	case types.HostRuleAllow:
		return false
	case types.HostRuleHold:
		return !p.holdHostCall(w, r, host, port)
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceHostRule)
	log.Cause = hostRuleCause(d)
	if p.sink != nil {
		p.sink.emit(log)
	}
	p.writeEgressDenyLLM(w, p.shapeOfHost(host), host, port, &log)
	return true
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
	return out
}
