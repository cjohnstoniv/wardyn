// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
)

// mitmSet is what decides whether a CONNECT is TLS-terminated: whether the proxy has a CA, whether
// the built-in LLM hosts are intended for MITM, and the compiled host and port tables.
type mitmSet struct {
	ca    bool
	llm   bool
	hosts map[string]bool
	ports map[string]int
}

// mitmSetOf compiles the set the proxy built from this config: the MITMHosts entries and the hosts
// each credential door adds to them, in the order newProxy adds them.
func (c *Config) mitmSetOf() mitmSet {
	hosts, ports, _ := compileMITMHosts(c.MITMHosts)
	newPATAPIGrants(canonicalPATGrants(c.PATGrants), hosts, ports)
	newGitHubAPIDoor(c.GitHubAPI, hosts, ports)
	newAzureARMDoor(c.AzureARM, hosts, ports)
	newAWSSigV4Door(c.AWSSigV4, hosts, ports)
	newKubeAPIDoor(c.KubeAPI, hosts, ports)
	return mitmSet{ca: c.MITMCACertPEM != "" && c.MITMCAKeyPEM != "", llm: c.MITMLLM, hosts: hosts, ports: ports}
}

func (m mitmSet) portAllowed(host string, port int) bool {
	cport := m.ports[hostKey(host)]
	return cport == 0 || cport == port
}

// terminates is the rule of handleConnect's two TLS-terminating branches: a CA, the port the entry
// names, and either a compiled host (a built-in LLM name is not one there) or a built-in LLM host
// with MITMLLM on.
func (m mitmSet) terminates(host string, port int) bool {
	h := hostKey(host)
	if !m.ca || !m.portAllowed(h, port) {
		return false
	}
	if h == anthropicHost || h == openaiHost {
		return m.llm
	}
	return m.hosts[h]
}

// TerminatesTLS reports whether a run with this config has its proxy open a CONNECT to host:port
// (terminate its TLS) rather than tunnel it unread. It is the decision of handleConnect's two
// branches (the corp host and the built-in LLM host, proxy.go) over the tables newProxy compiles,
// pinned against them by TestTerminatesTLSMatchesHandleConnect.
func (c *Config) TerminatesTLS(host string, port int) bool {
	return c.mitmSetOf().terminates(host, port)
}

// InspectsEntry reports whether one allowed_domains entry is Inspected: the proxy terminates the
// host it names (a bare entry reads as port 443), or, for a wildcard, terminates any host under it.
func (c *Config) InspectsEntry(entry string) bool {
	m := c.mitmSetOf()
	exact, wild, port := domainmatch.Classify(entry)
	if wild == "" {
		if port == 0 {
			port = 443
		}
		return m.terminates(exact, port)
	}
	candidates := make([]string, 0, len(m.hosts)+2)
	for h := range m.hosts {
		candidates = append(candidates, h)
	}
	candidates = append(candidates, anthropicHost, openaiHost)
	for _, h := range candidates {
		if !domainmatch.MatchWild(h, []string{wild}) {
			continue
		}
		p := port
		if p == 0 {
			// A bare wildcard allows every port, so the host's own configured port is inside it.
			if p = m.ports[h]; p == 0 {
				p = 443
			}
		}
		if m.terminates(h, p) {
			return true
		}
	}
	return false
}
