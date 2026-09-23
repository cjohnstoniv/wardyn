// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runAgentPolicy is the agent-side half of one run's autonomy level: the
// managed file dispatch hands the runner, and what run.agent_policy records
// about it. The zero value is "this run has no agent-side layer".
type runAgentPolicy struct {
	path  string
	bytes int
	// files is what SandboxSpec.ManagedFiles carries: the generated document,
	// or nil when the runner cannot deliver it.
	files []runner.ManagedFile
	// withheld says why the row records delivered:false: the document is not
	// on the spec, or it is but this runtime's delivery is not verified
	// (managedFilesGap). Empty when the delivery is one the row can vouch for.
	withheld string
	// execLess: the runner creates this run's agent container at Exec, not at
	// CreateSandbox (krun), so delivery — or the driver's refusal — happens
	// there, and the row waits for it.
	execLess bool
	// hold: the run launches on agent-run's hold lane, which gets a document
	// whatever its level (agentpolicy.ForAgent, #358).
	hold bool
}

// holdLane is whether this dispatch launches agent-run's hold lane: the one
// predicate the sandbox env (WARDYN_TOOL_APPROVALS) and the managed settings
// both read, so a run cannot get one without the other.
func (p dispatchParams) holdLane() bool {
	return !p.Interactive && p.ToolApprovals == "hold"
}

// agentPolicyBasis names what a run's managed settings were generated from,
// for a failure hint.
func agentPolicyBasis(level types.AutonomyLevel, hold bool) string {
	if level == "" && hold {
		return "tool approvals on hold"
	}
	return "autonomy level " + string(level)
}

// agentPolicyFor generates the agent-side half of this run's autonomy level
// (0.8 #95) and decides whether the sandbox gets it.
//
// Sited in dispatch rather than at create for the reason every other sandbox
// input is: the file is part of what the sandbox comes up with, and dispatch is
// the one place that composes that. It reads the LEVEL off the run row — the
// value resolveRunAutonomy froze there — so the file, the sandbox env
// (WARDYN_AUTONOMY_LEVEL) and the create audit row's `autonomy` block all cite
// one source.
//
// A Capabilities error fails the run (err != nil): whether this runner can
// place the file is unknown, and launching anyway would be the one silent
// route to a gated run with no managed layer — the same read fails a
// workspace launch closed.
//
// A runner that does not advertise Capabilities.ManagedFiles gets no file and
// the run still launches, under its CLI-flag levers alone. That is the
// decided fallback, not an oversight: refusing would make every L0–L2
// claude-code run undeployable on a substrate without the contract, and the
// row records delivered:false with the reason, so the gap is visible where an
// operator looks for it. Handing such a runner the file anyway would be worse
// than withholding it — a driver that cannot make it root-owned would place a
// ceiling the agent can rewrite, reported as delivered.
func (s *Server) agentPolicyFor(ctx context.Context, run types.AgentRun, hold bool) (runAgentPolicy, error) {
	path, content, ok := agentpolicy.ForAgent(run.Agent, run.AutonomyLevel, hold)
	if !ok {
		return runAgentPolicy{}, nil
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return runAgentPolicy{}, fmt.Errorf("the runner's capabilities could not be read, so whether it can deliver this run's managed settings (%s) is unknown: %w",
			agentPolicyBasis(run.AutonomyLevel, hold), err)
	}
	p := runAgentPolicy{path: path, bytes: len(content), hold: hold}
	var deliverable bool
	p.withheld, deliverable = managedFilesGap(caps, run.ConfinementClass)
	if !deliverable {
		return p, nil
	}
	p.files = []runner.ManagedFile{{Path: path, Content: content}}
	p.execLess = isExecLess(caps, run.ConfinementClass)
	return p, nil
}

// isExecLess reports an exec-less (krun) substrate for class: the same label
// byoiExecLessRefused reads.
func isExecLess(caps runner.Capabilities, class types.ConfinementClass) bool {
	return strings.HasPrefix(caps.Resolved[class], "oci/krun")
}

// krunManagedFilesUnverified is the ruling for the exec-less krun path: the
// file is still placed, but libkrun runs the guest init as root and does not
// apply the image's USER (the Docker driver's own note on that path), while
// the file's immutability rests on the agent NOT being root. Until that is
// verified on a krun host, the row does not vouch for it.
const krunManagedFilesUnverified = "unverified on this runtime: libkrun may run the guest as root"

// managedFilesGap is the one decision the audit row and the 201 warning both
// read, so they cannot drift: reason is why the row will record
// delivered:false ("" when it will not), and deliverable is whether the file
// goes on the spec at all.
func managedFilesGap(caps runner.Capabilities, class types.ConfinementClass) (reason string, deliverable bool) {
	switch {
	case !caps.ManagedFiles:
		return fmt.Sprintf("runner %q does not deliver managed files", caps.Driver), false
	case isExecLess(caps, class):
		return krunManagedFilesUnverified, true
	default:
		return "", true
	}
}

// managedSettingsUndeliveredWarning is the 201's half of delivered:false: the
// same fact the run.agent_policy row records, said where the person launching
// the run reads it. Empty when the level generates no file or the row will say
// delivered. A Capabilities error says nothing here: dispatch fails that run
// with the reason.
func (s *Server) managedSettingsUndeliveredWarning(ctx context.Context, agent string, level types.AutonomyLevel, hold bool, class types.ConfinementClass) string {
	if _, _, ok := agentpolicy.ForAgent(agent, level, hold); !ok || s.cfg.Runner == nil {
		return ""
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return ""
	}
	reason, _ := managedFilesGap(caps, class)
	if reason == "" {
		return ""
	}
	return fmt.Sprintf(
		"%s's managed settings for autonomy level %s are not delivered: %s, so this run's agent runs under its launch flags alone and a repository's own settings can let it run tools without asking",
		autonomyAgentLabel(agent), level, reason)
}

// auditAgentPolicy records run.agent_policy once the agent's container exists,
// so `delivered` is the driver's answer rather than dispatch's intent: the
// driver refuses the run outright when it cannot place the file root-owned
// (the Docker driver's image USER and /etc checks), so a container that exists
// with the file on its spec is a container that has it. That is after
// CreateSandbox on most runners, and after the agent's Exec on an exec-less
// one (runAgentPolicy.execLess).
//
// SILENT for a run with no agent-side layer, which is the ordinary case: an
// unrestricted or unbound level, and every agent but claude-code. A row saying
// "no file, and there was never going to be one" would put a governance event
// on every run on every deployment that has authored no rubric.
func (s *Server) auditAgentPolicy(ctx context.Context, run types.AgentRun, p runAgentPolicy, spec runner.SandboxSpec) {
	if p.path == "" {
		return
	}
	delivered := false
	for _, f := range spec.ManagedFiles {
		if f.Path == p.path && p.withheld == "" {
			delivered = true
		}
	}
	data := map[string]any{
		"agent": run.Agent,
		"level": string(run.AutonomyLevel),
		"path":  p.path,
		// The size, not the content: the document is not a secret, but a
		// governance row that carries a file body invites the next one to.
		// Enough to tell the three documents apart in a trail.
		"bytes":     p.bytes,
		"delivered": delivered,
	}
	if p.hold {
		// Says why a run with no level, or an L2/L3 one, got the gated document.
		data["tool_approvals"] = "hold"
	}
	if !delivered {
		reason := p.withheld
		if reason == "" {
			reason = "the sandbox spec did not carry the file"
		}
		data["reason"] = reason
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.agent_policy",
		run.ID.String(), "success", mustJSON(data)))
}
