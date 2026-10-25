// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runMacOSView is what the run page says of a macOS VM run beyond a Linux VM's desktop (mock M-D1):
// whether its Mac's Screen Sharing lets the desktop be controlled or only watched. A projection for
// display only, never a field of types.AgentRun.
type runMacOSView struct {
	// Desktop is runnerwire.MacVMDesktopControl or MacVMDesktopObserve, from the runner's last caps.
	Desktop string `json:"desktop"`
}

// runMacOSView reads a macOS VM run's view off its runner's live session: nil for any other run, a
// run on no runner of its own, a runner this replica holds no caps for, or a Mac whose base has no
// desktop at all. The desktop offer is the Mac's, not the run's, so it is read now, never stored.
func (s *Server) runMacOSView(run types.AgentRun) *runMacOSView {
	if !run.MachineKind.IsMacOSVM() || run.RunnerID == nil {
		return nil
	}
	s.runnerStreams.subsMu.Lock()
	ep, live := s.runnerStreams.subs[*run.RunnerID]
	s.runnerStreams.subsMu.Unlock()
	if !live {
		return nil
	}
	caps, ok := ep.sub.Caps()
	if !ok || caps.MacOSVM == nil {
		return nil
	}
	switch d := caps.MacOSVM.Desktop; d {
	case runnerwire.MacVMDesktopControl, runnerwire.MacVMDesktopObserve:
		return &runMacOSView{Desktop: d}
	}
	return nil
}
