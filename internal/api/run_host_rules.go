// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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
