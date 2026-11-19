// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// containDispatch is the containment loop's table, for its tests and logs: whether a round sends the run's
// substrate a call, and the reason. A hosted container, or a runner that serves the fence, is fenced. A
// runner without it, and a Kubernetes runner, get a proxy stop every round. Kubernetes in server mode and
// virtual machines get no call. caps are the run's runner's, ignored for a hosted run. cutAgent answers the
// same.
func (s *Server) containDispatch(run types.AgentRun, caps runnerwire.Caps) (call bool, reason string) {
	switch {
	case s.cfg.Runner == nil || run.SandboxRef == "":
		return false, "no sandbox to cut"
	case run.MachineKind.IsVM():
		return false, "a virtual machine is not cut from here"
	case run.RunnerID != nil:
		if _, can := s.cfg.Runner.(runner.ProxyFencer); can && caps.ProxyFence {
			return true, "fence"
		}
		if _, can := s.cfg.Runner.(runner.ProxyStopper); can {
			return true, "stop the proxy"
		}
	default:
		if s.cfg.Runner.Name() == "k8s" {
			return false, "Kubernetes in server mode cannot stop its proxy alone"
		}
		if _, can := s.cfg.Runner.(runner.ProxyFencer); can {
			return true, "fence"
		}
		if _, can := s.cfg.Runner.(runner.ProxyStopper); can {
			return true, "stop the proxy"
		}
	}
	return false, "the substrate can do neither"
}
