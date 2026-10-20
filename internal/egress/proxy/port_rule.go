// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "github.com/cjohnstoniv/wardyn/internal/egress"

// The port rule. A bare allowlist host, a wildcard, allow_all_egress, a trusted
// literal and a first-use approval reach the web ports (80, 443) only; another
// port needs an allowlist entry that names it ("host:port" or "*.suffix:port",
// the grant shape Policy.AuthoredPortFor already reads). On for every desktop
// VM run (Config.VM); for container runs it follows Config.PortRule, off by
// default, so a container run is unchanged until an operator turns it on.
const (
	PortRuleOff    = "off"
	PortRuleDryRun = "dry_run"
	PortRuleOn     = "on"

	ruleSourcePort = "policy:port"

	// portRuleDetail is the refusal's X-Wardyn-Egress-Detail and body sentence.
	portRuleDetail = "this run reaches an allowed host on ports 80 and 443 only; " +
		"an allowlist entry written host:port opens another port"
)

// ValidPortRule reports whether s is a port_rule value the sidecar accepts.
func ValidPortRule(s string) bool {
	switch s {
	case "", PortRuleOff, PortRuleDryRun, PortRuleOn:
		return true
	}
	return false
}

// effectivePortRule is the rule a proxy enforces: a VM run's is always on.
func effectivePortRule(vm bool, rule string) string {
	if vm {
		return PortRuleOn
	}
	if rule == "" {
		return PortRuleOff
	}
	return rule
}

// portRuleRefuses reports whether the rule, in either live mode, refuses
// host:port. Deny entries were already applied: AuthoredPortFor re-checks them.
func (p *Proxy) portRuleRefuses(host string, port int) bool {
	if p.portRule != PortRuleOn && p.portRule != PortRuleDryRun {
		return false
	}
	return port != 80 && port != 443 && !p.policy.AuthoredPortFor(host, port)
}

// markPortDryRun marks an allow the dry run would have refused.
func markPortDryRun(log *egress.DecisionLog, dryRun bool) {
	if dryRun {
		log.PortRule = PortRuleDryRun
	}
}
