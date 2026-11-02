// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// hostMarks says which of a run's allowed_domains entries its proxy opens (terminates TLS for) at
// launch. Every other allowed entry passes through unread. It is recorded on run.policy.resolve and
// served on the run's policy view; an envelope without it predates the mark and says nothing.
type hostMarks struct {
	Inspected []string `json:"inspected"` // never null
	// Terminated is every host the proxy terminates, as proxy.Config.TerminatedHosts lists them:
	// the allowlist's Inspected entries are a subset of what it covers.
	Terminated []string `json:"terminated"` // never null
	// Outside is the hosts the run's proxy would open (admit, or hold for first-use approval, and
	// terminate) that no allowed_domains entry names, by the proxy package's own decision
	// (proxy.Config.TerminatedOutside), so a reader lists them without matching hosts itself.
	Outside []string `json:"outside"` // never null
}

// hostMarksOf marks allowed over the proxy config dispatch is handing the sidecar, by the proxy's
// own rule (proxy.Config.InspectsEntry), never by the name of the host.
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
		if cfg.InspectsEntry(entry) {
			marks.Inspected = append(marks.Inspected, entry)
		}
	}
	return marks
}
