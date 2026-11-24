// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An MCP server that starts from a command (transport stdio) runs only in a sandbox of its own, never in the
// agent's. Whether a run may have one is decided by one table, mcpGateSteps, in order; each door asks the
// steps it can judge.

// envMCPOwnSandbox turns on MCP servers that start from a command, each in a sandbox of its own. Off, the
// default, every door refuses one (mcpOwnSandboxOff), and so does the organisation's Local MCP servers
// setting when it is off.
const envMCPOwnSandbox = "WARDYN_MCP_OWN_SANDBOX"

// mcpSandboxMax is the most MCP servers that start from a command one run may have.
const mcpSandboxMax = 4

// mcpOwnSandboxOff is the gate's second step: the switch or the organisation's setting is off, or the site
// config could not be read (readErr), which reads as off.
func mcpOwnSandboxOff(sc types.SiteConfig, readErr error) bool {
	return !envEnabled(envMCPOwnSandbox) || readErr != nil || !localMCPOn(sc)
}

// mcpGateFacts is what the gate reads about a run. Each door fills what it knows.
type mcpGateFacts struct {
	stdio int               // the run's MCP servers that start from a command
	off   bool              // step 2: mcpOwnSandboxOff
	kind  types.MachineKind // the run's machine kind, normalized
	local bool              // the run lands on the local placement
	kinds []types.MachineKind
}

// The doors: the save, admission and revive doors judge the switch alone; the fold judges the rest.
type mcpGateDoor int

const (
	mcpAtSwitch mcpGateDoor = iota
	mcpAtFold
)

// mcpGateStep is one row of the gate: where it is judged, what it answers, and when.
type mcpGateStep struct {
	door     mcpGateDoor
	reason   string
	sentence string
	refuses  func(mcpGateFacts) bool
}

// mcpGateSteps is the gate, in order. Step 1, a definition that is not stdio passes, is mcpGate's own. A
// Linux VM run is always on the local placement, so its own row comes before the local one.
var mcpGateSteps = []mcpGateStep{
	{mcpAtSwitch, reasonMCPOwnSandboxOff, mcpOwnSandboxOffSentence, func(f mcpGateFacts) bool { return f.off }},
	{mcpAtFold, reasonMCPOwnSandboxUnsupported, mcpOwnSandboxMacSentence, func(f mcpGateFacts) bool { return f.kind == types.MachineMacOSVM }},
	{mcpAtFold, reasonMCPOwnSandboxUnsupported, mcpOwnSandboxLinuxVMSentence, func(f mcpGateFacts) bool { return f.kind == types.MachineLinuxVM }},
	{mcpAtFold, reasonMCPOwnSandboxUnsupported, mcpOwnSandboxUnsupportedSentence, func(f mcpGateFacts) bool {
		return f.local || !slices.Contains(f.kinds, f.kind)
	}},
	{mcpAtFold, reasonMCPSandboxLimit, mcpSandboxLimitSentence, func(f mcpGateFacts) bool { return f.stdio > mcpSandboxMax }},
}

// mcpGate is the first step of door that refuses f, nil when none does or the run has no stdio server.
func mcpGate(door mcpGateDoor, f mcpGateFacts) *mcpGateStep {
	if f.stdio == 0 {
		return nil
	}
	for i := range mcpGateSteps {
		if st := &mcpGateSteps[i]; st.door == door && st.refuses(f) {
			return st
		}
	}
	return nil
}

// mcpSaveProblem is the component save's step 2: "" unless def starts from a command and the switch or the
// setting is off, or the site config cannot be read.
func (s *Server) mcpSaveProblem(ctx context.Context, def types.ComponentDefinition) string {
	if !def.LocalMCP() {
		return ""
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if mcpGate(mcpAtSwitch, mcpGateFacts{stdio: 1, off: mcpOwnSandboxOff(sc, err)}) != nil {
		return mcpSaveOffSentence
	}
	return ""
}

// mcpAdmissionRefusal is the admission door's step 2, on the site config the component gate already read
// (a read error is answered before it).
func mcpAdmissionRefusal(site types.SiteConfig, c attachedComponent, i int) *componentRefusal {
	if !c.snapshot.Definition.LocalMCP() {
		return nil
	}
	st := mcpGate(mcpAtSwitch, mcpGateFacts{stdio: 1, off: mcpOwnSandboxOff(site, nil)})
	if st == nil {
		return nil
	}
	return &componentRefusal{
		refusal: runError(http.StatusUnprocessableEntity, st.reason, mcpSandboxSentence(st.sentence, mcpSlug(c.snapshot.Name), "")),
		reason:  st.reason, ordinal: i, source: c.source, id: c.snapshot.ComponentID,
	}
}

// stepMCPSandbox is the fold's part of the gate: the machine kind, the placement and the executor's kinds,
// and the count. It runs after the components were admitted (step 2) and after the placement was decided.
func (s *Server) stepMCPSandbox(f *runFold, _ *foldRecorder) bool {
	return !s.mcpSandboxRefusal(f.r.Context(), *f.req, f.place, f.comps).write(s, f.w, f.r)
}

// mcpSandboxRefusal is stepMCPSandbox's answer. A local run is refused without a capability read; a remote
// one reads the deployment's executor once, whose kinds are already intersected across its executors.
func (s *Server) mcpSandboxRefusal(ctx context.Context, req createRunRequest, place runPlacement, comps runComponents) *runRefusal {
	facts := mcpGateFacts{kind: types.MachineKind(req.MachineKind).Normalize()}
	keys, server := mcpServerKeys(comps.attached), ""
	for i, a := range comps.attached {
		if a.snapshot.Definition.LocalMCP() {
			if facts.stdio == 0 {
				server = keys[i]
			}
			facts.stdio++
		}
	}
	if facts.stdio == 0 {
		return nil
	}
	p := requestPlacement(req)
	if place.pooled {
		p = place.placement
	}
	facts.local = p == types.PlacementLocal
	if !facts.local && !facts.kind.IsVM() {
		pc, err := s.placementCapabilities(ctx, types.PlacementRemote, uuid.Nil)
		if err != nil {
			return runServerError("read runner capabilities", err)
		}
		facts.kinds = pc.MCPSandboxKinds
	}
	st := mcpGate(mcpAtFold, facts)
	if st == nil {
		return nil
	}
	runnerName := ""
	if st.sentence == mcpOwnSandboxUnsupportedSentence {
		runnerName = s.mcpRunnerName(ctx, req, place)
	}
	return runError(http.StatusUnprocessableEntity, st.reason, mcpSandboxSentence(st.sentence, server, runnerName))
}

// mcpRunnerName is the run's runner or executor name for the unsupported-runner sentence, else "This runner".
func (s *Server) mcpRunnerName(ctx context.Context, req createRunRequest, place runPlacement) string {
	run := types.AgentRun{Placement: requestPlacement(req)}
	switch {
	case !place.pooled:
		if id := requestRunnerID(req); id != uuid.Nil {
			run.RunnerID = &id
		}
	case len(place.eligible) == 1:
		run.Placement, run.RunnerID, run.RunnerPoolExecutor = place.placement, place.eligible[0].runnerID, place.eligible[0].executor
	}
	if name := s.runnerNameOf(ctx, run); name != "" {
		return name
	}
	return mcpRunnerUnnamed
}
