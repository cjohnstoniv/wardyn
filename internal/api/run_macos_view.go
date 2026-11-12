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

// macOSDesktopViewOnly says a run's desktop stream keeps the viewer's input from the guest: every macOS VM
// run unless its runner's live caps say the Mac's Screen Sharing can be controlled, and always while an
// attended window is open on the run (run_mac_window.go).
func (s *Server) macOSDesktopViewOnly(run types.AgentRun) bool {
	if !run.MachineKind.IsMacOSVM() {
		return false
	}
	if s.macWindows.open(run.ID) != nil {
		return true // one keyboard at a time: while a window is open, the browser desktop only watches
	}
	v := s.runMacOSView(run)
	return v == nil || v.Desktop != runnerwire.MacVMDesktopControl
}

// macOSLiveDesktop is a macOS VM run's Mac's desktop offer as its connected runner states it now, for the
// question that opens a door, where no answer is never a yes: connected is false on no runner, a runner this
// replica holds no session for, or one that is offline, whose remembered caps say nothing. The offer is ""
// when a connected runner's caps name no macOS VM.
func (s *Server) macOSLiveDesktop(run types.AgentRun) (offer string, connected bool) {
	if !run.MachineKind.IsMacOSVM() || run.RunnerID == nil {
		return "", false
	}
	s.runnerStreams.subsMu.Lock()
	ep, live := s.runnerStreams.subs[*run.RunnerID]
	s.runnerStreams.subsMu.Unlock()
	if !live {
		return "", false
	}
	if _, online := ep.sub.Advertised(); !online {
		return "", false
	}
	caps, ok := ep.sub.Caps()
	if !ok || caps.MacOSVM == nil {
		return "", true
	}
	return caps.MacOSVM.Desktop, true
}
