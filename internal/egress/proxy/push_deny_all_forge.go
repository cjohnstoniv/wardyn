// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// push_rules.deny_all is enforced where Wardyn brokers the traffic: the git
// broker lanes, the Azure DevOps REST door and the GitHub API door. A code host
// a policy allows as a plain domain is otherwise an opaque tunnel, and a push
// token kept as an environment or file secret can push through it. So while
// deny_all is in force, evaluate refuses every connection to a code host Wardyn
// knows, on any port and on both lanes, before the allowlist, allow_all_egress
// or a first-use approval can admit it. A host one of the run's doors serves is
// left to that door, which terminates it or refuses it on every port.

// ruleSourceGitPushDenyAllForge marks a connection to a known code host refused
// because the run's push_rules set deny_all and no Wardyn door serves the host.
const ruleSourceGitPushDenyAllForge = "brokered:git:push-deny-all-forge"

// pushDenyAllForgeRefusal is the sentence the refusal carries.
func pushDenyAllForgeRefusal(host string) string {
	return "Wardyn refused this connection: this run may not push, and " + host +
		" is a code host Wardyn doesn't broker for this run."
}

// publicForgeHosts is types.PublicForgeHosts by key; adoHostedHost adds the
// rest of hosted Azure DevOps (*.dev.azure.com and *.visualstudio.com).
var publicForgeHosts = forgeHostSet(types.PublicForgeHosts)

// adoExtensionGallery is the editor extension gallery: under *.visualstudio.com
// but not a code host, so the guard leaves it reachable. Every other
// *.visualstudio.com host (an Azure DevOps organization's older address) stays
// refused.
const adoExtensionGallery = "marketplace.visualstudio.com"

// forgeHostSet compiles Config.ForgeHosts: the code hosts dispatch knows from
// site config and the run's git_pat and ssh_key grants, and the public ones.
// Nil when there are none.
func forgeHostSet(hosts []string) map[string]bool {
	var out map[string]bool
	for _, h := range hosts {
		if k := forgeHostKey(h); k != "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[k] = true
		}
	}
	return out
}

// forgeHostKey is host as the guard compares it: lower case, no port, no
// trailing dot, and an internationalised name in its ASCII (punycode) form, so
// no other spelling of a known host reaches the wire past the guard.
func forgeHostKey(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = domainmatch.CanonHost(strings.TrimRight(strings.TrimSpace(host), "."))
	for i := 0; i < len(host); i++ {
		if host[i] >= utf8.RuneSelf {
			if a, err := idna.Lookup.ToASCII(host); err == nil {
				return strings.TrimRight(a, ".")
			}
			break
		}
	}
	return host
}

// pushDenyAllInForce reports whether any push rules the run carries set
// deny_all: the run-wide block, the base an entry with no set is held to, or
// any entry's set. A tunnel is not bound to one entry, so one is enough.
func (p *Proxy) pushDenyAllInForce() bool {
	if rs := p.policy.contentRules(); rs != nil && rs.denyAll {
		return true
	}
	if p.pushBase != nil && p.pushBase.denyAll {
		return true
	}
	for _, s := range p.pushSets {
		if s.Rules.DenyAll {
			return true
		}
	}
	return false
}

// knownForgeHost reports whether key (forgeHostKey's form) is a code host
// Wardyn knows for this run: a public one, one dispatch named, or one the
// run's own broker lanes, held pushes, resident mints or push rule sets name.
func (p *Proxy) knownForgeHost(key string) bool {
	if publicForgeHosts[key] || (adoHostedHost(key) && key != adoExtensionGallery) || p.forgeHosts[key] {
		return true
	}
	if _, ok := p.patGrants[key]; ok {
		return true
	}
	if _, ok := p.gitPushWithheld[key]; ok {
		return true
	}
	for _, m := range p.residentMints {
		if m.Host != "" && forgeHostKey(m.Host) == key {
			return true
		}
	}
	for _, s := range p.pushSets {
		if h, err := types.CanonicalPushHost(s.Provider, s.Host); err == nil && h == key {
			return true
		}
	}
	return false
}

// forgeDoorHost reports whether one of the run's doors serves host: the Azure
// DevOps REST gate, the GitHub API door or the git_pat API door. Each
// terminates its host and refuses a tunnel or a plain request to it on any
// other terms (refuseGrantHostTunnel, servePlain), so it is never an opaque
// tunnel, and the first two refuse repository writes under deny_all.
func (p *Proxy) forgeDoorHost(host string) bool {
	return p.isADOLane(host) || p.isGitHubAPIHost(host) || p.isPATAPIHost(host)
}

// pushDenyAllForgeDeny is evaluate's deny_all step: the refusal for a
// connection to a known code host no door serves while deny_all is in force,
// nil otherwise. It runs only while the host verdict can still end in an allow,
// so a host the policy refuses anyway keeps that refusal.
func (p *Proxy) pushDenyAllForgeDeny(req egress.Request, host string, v egress.HostVerdict) *egress.DecisionLog {
	if !p.policyMayAdmit(v) || !p.pushDenyAllInForce() || p.forgeDoorHost(host) {
		return nil
	}
	if key := forgeHostKey(host); !p.knownForgeHost(key) {
		return nil
	}
	log := decisionLog(req, egress.Deny, ruleSourceGitPushDenyAllForge)
	log.Cause = pushDenyAllForgeRefusal(host)
	return &log
}

// policyMayAdmit reports whether the host verdict can still end in an allow:
// an allow, or an unknown host the run's first-use approval may release.
func (p *Proxy) policyMayAdmit(v egress.HostVerdict) bool {
	return v == egress.VerdictAllow || (v == egress.VerdictUnknown && p.policy.FirstUseMode().RaisesApproval() && p.approval != nil)
}
