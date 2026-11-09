// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// withholdUnboundDispatchInjections withholds every credential the run's proxy would refuse to start with, and
// returns their hosts. The proxy starts only with an injection its policy binds exactly
// (buildInjectorFor), and a policy can allow a host without binding it: by a wildcard or
// allow_all_egress, or, after a layer's allowlist, by another port of the host only (layerWithheld
// reads both as allowing it). Each such credential is withheld here, read from the policy the sidecar
// starts with, so the run starts without it; settleCredentialHosts drops its interception entry and
// still counts it in the one-credential-per-host check (p.unboundInjections). A remote MCP server's
// credential is the broker's, on a host no allowlist names (rawEgressHosts), so dispatchMCP keeps
// deciding it.
func withholdUnboundDispatchInjections(policy *types.RunPolicySpec, p *dispatchParams, injections *[]runner.InjectionGrant) []string {
	bound := proxy.CompilePolicy(sidecarPolicy(*policy, *p)).AllowedExactHost
	var dropped []string
	p.unboundInjections = nil
	*injections = slices.DeleteFunc(slices.Clone(*injections), func(in runner.InjectionGrant) bool {
		if bound(strings.TrimSpace(in.Rule.Host)) || brokeredMCPRule(p.Components.MCP, in.Rule.Host) { // as buildInjectorFor reads it
			return false
		}
		p.unboundInjections = append(p.unboundInjections, in)
		dropped = append(dropped, in.Rule.Host)
		return true
	})
	return dropped
}

// brokeredMCPRule reports whether an injection on host is a remote MCP server's credential, the one
// dispatchMCP moves to the broker entry (it pairs them by host as here).
func brokeredMCPRule(servers []componentMCP, host string) bool {
	return slices.ContainsFunc(servers, func(m componentMCP) bool { return m.Launch == nil && m.Credentialed && hostEqual(m.Host, host) })
}

// ceilingReassertNote is the run.ceiling.reassert row's fixed sentence: why its credentials were
// withheld. "" when the row records no action taken (acted is false and no other case holds), because a
// note in the indicative reads as a report of action.
func ceilingReassertNote(profile string, unboundOnly, boundaryLayer, acted bool) string {
	switch {
	case unboundOnly:
		return "the policy this run's proxy starts with does not allow these hosts exactly, so their credentials are withheld instead of stopping the proxy's start"
	case profile == "" && boundaryLayer:
		return "the Wardyn boundary denies these hosts or does not allow them, so every credential lane that reaches one is withheld"
	case profile == "":
		return "a restriction, the run owner's walls or the run's wall preset deny these hosts or keep these credentials out, so every credential lane that reaches one is withheld"
	case acted:
		return "the acting principal's governance profile denies these hosts; the denies are unioned into the run policy " +
			"(deny beats allow and allow_all_egress at the proxy) and every credential lane that reaches a denied host is withheld — " +
			"the brokered git/PAT routes mint proxy-side and never consult denied_domains, so dropping the lane is the only thing that binds them"
	}
	return ""
}
