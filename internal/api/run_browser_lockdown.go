// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// browserLockdown returns the Chromium managed policy WARDYN_BROWSER_LOCKDOWN binds every run's
// browser to (#2970), as a managed file the agent cannot rewrite; nothing when the lockdown is off.
// A run whose runner cannot deliver it where the agent cannot rewrite it is refused, and so is a
// macOS VM run, whose guest has no place for it: a lockdown the run shows and nothing enforces is a
// wall that is not there.
func (s *Server) browserLockdown(ctx context.Context, run types.AgentRun) ([]runner.ManagedFile, error) {
	if s.cfg.BrowserPolicy == nil {
		return nil, nil
	}
	refused := "This run was not launched: this deployment locks down the sandbox browser, and "
	if run.MachineKind == types.MachineMacOSVM {
		return nil, agentPolicyRefusal(refused + "a macOS virtual machine cannot take the browser policy yet")
	}
	caps, err := s.runCapabilities(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agentPolicyRefusal(refused+"its runner's capabilities could not be confirmed"), err)
	}
	if reason, deliverable := managedFilesGap(caps, run.ConfinementClass); !deliverable || reason != "" {
		return nil, agentPolicyRefusal(refused + "this runner cannot vouch for the browser policy: " + reason)
	}
	return []runner.ManagedFile{{Path: runner.ChromiumPolicyFile, Content: s.cfg.BrowserPolicy}}, nil
}
