// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stepHostRulesVM refuses host rules on a VM run (422 host_rules_need_inspected_vm) at every door. A VM
// guest's flows reach the proxy through the guest relay, and no test yet proves such a flow to a rule
// host is opened or refused rather than tunnelled unopened; the proxy refuses the config as well
// (proxy.Config.validateHostRules). It runs after the tool policy, once the spec holds every layer's sets.
func (s *Server) stepHostRulesVM(f *runFold, _ *foldRecorder) bool {
	if len(f.spec.HostRules) == 0 || !types.MachineKind(f.req.MachineKind).IsVM() {
		return true
	}
	return !runError(http.StatusUnprocessableEntity, reasonHostRulesNeedInspectedVM,
		"This run's policy has host rules, and host rules are not yet judged on a VM run. Launch it in a container, or remove host_rules.").write(s, f.w, f.r)
}

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
