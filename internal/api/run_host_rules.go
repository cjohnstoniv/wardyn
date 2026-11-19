// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hostRulesNeedCA reports whether the run's host rules need the per-run CA: the proxy judges a rule host's
// requests only inside a connection it opens, and refuses a CONNECT to one without a CA.
func hostRulesNeedCA(policy *types.RunPolicySpec) bool {
	return policy != nil && len(policy.HostRules) > 0
}

// hostRuleMITMHosts is a "host:443" interception entry for each host the run's host rules name, so the
// proxy opens that port for it and refuses any other (policy:host-rule-needs-inspection). A host only a
// set the seal adds names gets no entry: the proxy opens it on any port when the run has a CA and refuses
// it without one, so it is never tunnelled unopened either way.
func hostRuleMITMHosts(policy *types.RunPolicySpec) []string {
	if policy == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range policy.HostRules {
		for _, r := range s.Rules {
			if !seen[r.Host] {
				seen[r.Host] = true
				out = append(out, net.JoinHostPort(r.Host, "443"))
			}
		}
	}
	return out
}
