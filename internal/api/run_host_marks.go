// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// hostMarks says which of a run's allowed_domains entries its proxy opens (terminates TLS for) at
// launch. Every other allowed entry passes through unread. It is recorded on run.policy.resolve and
// served on the run's policy view; an envelope without it predates the mark and says nothing.
type hostMarks struct {
	Inspected []string `json:"inspected"` // never null
}

// hostMarksOf marks allowed over the proxy config dispatch is handing the sidecar, by the proxy's
// own rule (proxy.Config.InspectsEntry), never by the name of the host.
func hostMarksOf(pc runner.ProxyConfig, allowed []string) *hostMarks {
	cfg := proxy.Config{
		MITMCACertPEM: pc.MITMCACertPEM, MITMCAKeyPEM: pc.MITMCAKeyPEM,
		MITMHosts: pc.MITMHosts, MITMLLM: pc.MITMLLM, PATGrants: pc.PATGrants,
		GitHubAPI: pc.GitHubAPI, AzureARM: pc.AzureARM, AWSSigV4: pc.AWSSigV4, KubeAPI: pc.KubeAPI,
	}
	marks := &hostMarks{Inspected: []string{}}
	for _, entry := range allowed {
		if cfg.InspectsEntry(entry) {
			marks.Inspected = append(marks.Inspected, entry)
		}
	}
	return marks
}
