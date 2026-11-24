// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net"
	"strconv"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// hostMarks says which of a run's allowed_domains entries its proxy opens (terminates TLS for) at
// launch. Every other allowed entry passes through unread. It is recorded on run.policy.resolve and
// served on the run's policy view; an envelope without it predates the mark and says nothing.
type hostMarks struct {
	// Inspected is the entries whose every destination the proxy opens. An entry it opens only some of is
	// in Partial instead, never in both.
	Inspected []string `json:"inspected"` // never null
	// Partial maps an entry the proxy opens only some destinations of (a wildcard, or a host with no port
	// opened on one port) to those destinations, as Terminated lists them: the rest the entry allows passes
	// through unread. Absent when no entry is partial, and in an envelope that predates it.
	Partial map[string][]string `json:"partial,omitempty"`
	// Terminated is every host the proxy terminates, as proxy.Config.TerminatedHosts lists them:
	// the allowlist's Inspected entries are a subset of what it covers.
	Terminated []string `json:"terminated"` // never null
	// Outside is the hosts the run's proxy would open (admit, or hold for first-use approval, and
	// terminate) that no allowed_domains entry names, by the proxy package's own decision
	// (proxy.Config.TerminatedOutside), so a reader lists them without matching hosts itself.
	Outside []string `json:"outside"` // never null
}

// hostMarksOf marks allowed over the proxy config dispatch is handing the sidecar, by the proxy's
// own rule (proxy.Config.InspectsEntry and InspectedSubset), never by the name of the host.
func hostMarksOf(pc runner.ProxyConfig, allowed []string) *hostMarks {
	cfg := runner.ProxyConfigOf(uuid.Nil, pc, runner.ProxyListenPort)
	// Under allow-all a reader shows no allowlist rows, only Outside, so no entry names a host there:
	// dispatch still appends hosts such as the GitHub CLI's api.github.com:443 to allowed_domains.
	named := allowed
	if pc.Policy.AllowAllEgress {
		named = nil
	}
	marks := &hostMarks{Inspected: []string{}, Terminated: cfg.TerminatedHosts(), Outside: cfg.TerminatedOutside(named)}
	for _, entry := range allowed {
		if subset, partial := cfg.InspectedSubset(entry); partial {
			if marks.Partial == nil {
				marks.Partial = map[string][]string{}
			}
			marks.Partial[entry] = subset
		} else if cfg.InspectsEntry(entry) {
			marks.Inspected = append(marks.Inspected, entry)
		}
	}
	return marks
}

// scopedHostMarks is marks as a run's view serves them. An envelope recorded before partial existed marked a
// wildcard, or a host with no port, Inspected when the proxy opened any host or port it names. Each such
// entry is moved to Partial with the destinations the envelope's own terminated list shows it opens: a
// wildcard always (it names hosts the proxy never opens), a host with no port unless terminated lists the
// host on every port. An entry the list cannot place gets an empty list: some of it is opened, which ones
// is not known. An envelope that has partial is returned as it is.
func scopedHostMarks(m *hostMarks) *hostMarks {
	if m == nil || m.Partial != nil {
		return m
	}
	out := *m
	out.Inspected = []string{}
	for _, entry := range m.Inspected {
		exact, wild, port := domainmatch.Classify(entry)
		if wild == "" && port != 0 {
			out.Inspected = append(out.Inspected, entry) // a host and port named: opened whole
			continue
		}
		subset, whole := []string{}, false
		for _, t := range m.Terminated {
			host, tport := splitTerminatedHost(t)
			switch {
			case wild != "":
				if domainmatch.MatchWild(host, []string{wild}) && (port == 0 || tport == 0 || tport == port) {
					subset = append(subset, t)
				}
			case host == exact && tport == 0:
				whole = true
			case host == exact:
				subset = append(subset, t)
			}
		}
		if whole {
			out.Inspected = append(out.Inspected, entry)
			continue
		}
		if out.Partial == nil {
			out.Partial = map[string][]string{}
		}
		out.Partial[entry] = subset
	}
	return &out
}

// splitTerminatedHost is a terminated entry as TerminatedHosts lists it: host, host:port or [v6]:port.
func splitTerminatedHost(t string) (string, int) {
	if h, p, err := net.SplitHostPort(t); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return h, n
		}
	}
	return t, 0
}
