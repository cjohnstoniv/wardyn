// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
)

// mitmSet is what decides whether a CONNECT is TLS-terminated: whether the proxy has a CA, whether
// the built-in LLM hosts are intended for MITM, the compiled host and port tables, and the hosts a
// via_org route covers.
type mitmSet struct {
	ca     bool
	llm    bool
	hosts  map[string]bool
	ports  map[string]int
	viaOrg map[string]bool
}

// mitmSetOf compiles the set the proxy built from this config: the tables newProxy builds
// (buildMITMTables) and the via_org hosts.
func (c *Config) mitmSetOf() mitmSet {
	t := buildMITMTables(c.MITMHosts, canonicalPATGrants(c.PATGrants), c.GitHubAPI, c.AzureARM, c.AWSSigV4, c.KubeAPI)
	viaOrg := make(map[string]bool, len(c.ViaOrgRoutes))
	for _, r := range c.ViaOrgRoutes {
		viaOrg[hostKey(r.Host)] = true
	}
	return mitmSet{ca: c.MITMCACertPEM != "" && c.MITMCAKeyPEM != "", llm: c.MITMLLM, hosts: t.hosts, ports: t.ports, viaOrg: viaOrg}
}

func (m mitmSet) portAllowed(host string, port int) bool {
	cport := m.ports[hostKey(host)]
	return cport == 0 || cport == port
}

// terminates is the rule of the three places the proxy terminates TLS: a via_org host on any port
// (serveViaOrg), and handleConnect's two branches, each needing the port the entry names: a
// compiled host (a built-in LLM name is not one there) or a built-in LLM host with MITMLLM on. All
// need a CA.
func (m mitmSet) terminates(host string, port int) bool {
	h := hostKey(host)
	if !m.ca {
		return false
	}
	if m.viaOrg[h] {
		return true
	}
	if !m.portAllowed(h, port) {
		return false
	}
	return m.terminatesHost(h)
}

// terminatesHost is terminates without the port: whether the host is terminated on the port its
// entry names (any port when the entry names none).
func (m mitmSet) terminatesHost(h string) bool {
	if h == anthropicHost || h == openaiHost {
		return m.llm
	}
	return m.hosts[h]
}

// terminatesAnyPort reports whether the proxy terminates h on at least one port.
func (m mitmSet) terminatesAnyPort(h string) bool {
	return m.ca && (m.viaOrg[h] || m.terminatesHost(h))
}

// terminated lists every host the proxy terminates: host when on any port, host:port when scoped to one.
func (m mitmSet) terminated() []string {
	if !m.ca {
		return []string{}
	}
	out := []string{}
	add := func(h string) {
		if m.viaOrg[h] || m.ports[h] == 0 {
			out = append(out, h)
			return
		}
		out = append(out, h+":"+strconv.Itoa(m.ports[h]))
	}
	for h := range m.viaOrg {
		add(h)
	}
	for h := range m.hosts {
		if m.terminatesHost(h) {
			add(h)
		}
	}
	for _, h := range []string{anthropicHost, openaiHost} {
		if m.llm {
			add(h)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TerminatesTLS reports whether a run with this config has its proxy open a CONNECT to host:port
// (terminate its TLS) rather than tunnel it unread. It is the decision of the three places the proxy
// terminates (handleConnect's corp host and built-in LLM host branches, and a via_org route) over the
// tables newProxy compiles, pinned against them by TestTerminatesTLSMatchesHandleConnect.
func (c *Config) TerminatesTLS(host string, port int) bool {
	return c.mitmSetOf().terminates(host, port)
}

// TerminatedHosts lists every host a run with this config has its proxy terminate, sorted: host when it
// is terminated on any port (a via_org host, or a compiled host with no port), host:port when
// a compiled host is scoped to one. It is empty without a CA.
func (c *Config) TerminatedHosts() []string {
	return c.mitmSetOf().terminated()
}

// TerminatedOutside lists, in TerminatedHosts order, the hosts the run's proxy would open on a port no
// allowed_domains entry names for them: a via_org host (ServeHTTP routes it before any policy decision),
// or a terminated host the proxy's own CONNECT decision (gateConnect) does not refuse there, so one it
// admits or holds for first-use approval and opens once approved. A host the proxy refuses on every such
// port (a denied host, a host the allowlist lacks when it asks nobody, a port the port rule closes) is not
// listed. An entry names a host on its own port, or on every port when it has none; a wildcard entry
// names the hosts it matches. A host terminated on any port is checked on outsideProbePorts.
func (c *Config) TerminatedOutside(allowed []string) []string {
	m := c.mitmSetOf()
	pol := CompilePolicy(c.Policy)
	gate := builtinEvaluator{p: pol}
	portRule := effectivePortRule(c.VM, c.PortRule)
	canAsk := pol.FirstUseMode().RaisesApproval()
	out := []string{}
	for _, t := range m.terminated() {
		host, ports := t, c.outsideProbePorts()
		if h, p, ok := strings.Cut(t, ":"); ok {
			n, _ := strconv.Atoi(p)
			host, ports = h, []int{n}
		}
		if slices.ContainsFunc(ports, func(port int) bool {
			if entryNames(allowed, host, port) {
				return false
			}
			req := egress.Request{Host: host, Port: port, Method: http.MethodConnect}
			return m.viaOrg[host] || gateConnect(context.Background(), req, gate, pol, portRule).outcome(canAsk) != connectRefuse
		}) {
			out = append(out, t)
		}
	}
	return out
}

// entryNames reports whether an allowed_domains entry names host on port: an exact entry for host or a
// wildcard matching it, with no port or with that port.
func entryNames(allowed []string, host string, port int) bool {
	return slices.ContainsFunc(allowed, func(entry string) bool {
		exact, wild, eport := domainmatch.Classify(entry)
		return (exact == host || wild != "" && domainmatch.MatchWild(host, []string{wild})) && (eport == 0 || eport == port)
	})
}

// outsideProbePorts is every port on which the CONNECT decision for a host can differ: 80, 443, each port
// an allowed_domains or denied_domains entry names, and one port none names, which stands for all the
// others (the policy and the port rule decide them alike).
func (c *Config) outsideProbePorts() []int {
	named := map[int]bool{80: true, 443: true}
	for _, d := range slices.Concat(c.Policy.AllowedDomains, c.Policy.DeniedDomains) {
		if _, _, p := domainmatch.Classify(d); p > 0 {
			named[p] = true
		}
	}
	other := 1
	for named[other] {
		other++
	}
	named[other] = true
	return slices.Sorted(maps.Keys(named))
}

// InspectsEntry reports whether one allowed_domains entry is Inspected: the proxy terminates the
// host it names (a bare entry names every port, so any port the proxy terminates it on counts), or, for
// a wildcard, terminates any host under it.
func (c *Config) InspectsEntry(entry string) bool {
	m := c.mitmSetOf()
	exact, wild, port := domainmatch.Classify(entry)
	if wild == "" {
		if port == 0 {
			return m.terminatesAnyPort(hostKey(exact))
		}
		return m.terminates(exact, port)
	}
	candidates := make([]string, 0, len(m.hosts)+len(m.viaOrg)+2)
	for h := range m.hosts {
		candidates = append(candidates, h)
	}
	for h := range m.viaOrg {
		candidates = append(candidates, h)
	}
	candidates = append(candidates, anthropicHost, openaiHost)
	for _, h := range candidates {
		if !domainmatch.MatchWild(h, []string{wild}) {
			continue
		}
		// A bare wildcard allows every port, so any port the proxy terminates the host on is inside it.
		if port == 0 && m.terminatesAnyPort(h) || port != 0 && m.terminates(h, port) {
			return true
		}
	}
	return false
}
