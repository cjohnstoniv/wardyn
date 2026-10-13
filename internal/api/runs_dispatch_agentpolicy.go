// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runAgentPolicy is the agent-side half of one run's autonomy level: the
// managed file dispatch hands the runner, and what run.agent_policy.write records
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
	// locked: the document ForAgent chose is the locked L2 one (agentpolicy.IsLocked).
	locked bool
	// toolRules counts the distinct tool names the interactive run's rules
	// wrote into the document (agentpolicy.WithToolRules); 0 when none.
	toolRules int
}

// agentPolicyRefusal is an agentPolicyFor error that is the run's failure hint
// as it stands: it names the run's own tool rules or its runner's missing
// capability, never a driver's error text.
type agentPolicyRefusal string

func (e agentPolicyRefusal) Error() string { return string(e) }

// agentPolicyFailureHint is what a run agentPolicyFor refused is told.
func agentPolicyFailureHint(err error, level types.AutonomyLevel, hold, locked bool) string {
	var refusal agentPolicyRefusal
	if errors.As(err, &refusal) {
		return string(refusal)
	}
	return fmt.Sprintf("This run was not launched: its runner's capabilities could not be confirmed, "+
		"so whether it can deliver this run's managed settings (%s) is unknown", agentPolicyBasis(level, hold, locked))
}

// attendedToolRules are the tool rules Claude Code enforces itself in an
// interactive run, through its managed settings: its own set when the run
// carries one per harness, else the run's. Nil off the interactive lane, where
// the gated lane's proxy enforces them, or nothing does.
func (p dispatchParams) attendedToolRules() []types.ToolRule {
	if !p.Interactive {
		return nil
	}
	if i := slices.IndexFunc(p.HarnessToolRules, func(h types.HarnessToolRules) bool { return h.Harness == claudeCodeHarness }); i >= 0 {
		return p.HarnessToolRules[i].Rules
	}
	if len(p.HarnessToolRules) > 0 {
		return nil
	}
	return p.Policy.ToolRules
}

// holdLane is whether a run launches agent-run's hold lane: the one predicate
// the sandbox env (WARDYN_TOOL_APPROVALS), the managed settings and the 201's
// undelivered warning all read, so a run cannot get one without the other.
//
// Excludes task_mode=exec: maybe_exec_task_mode (agent-run-lib.sh) execs the
// plain shell command and never reaches the harness launch or its
// WARDYN_TOOL_APPROVALS check, so an exec run has no agent process to hold —
// delivering the hold lane's L1 managed-settings file to one anyway (#358's
// hold&&HoldTakesOver case, for a level that alone brings no document) hands a
// custom image's root-owned USER a file no agent reads, and the Docker driver
// refuses the launch outright (managed_files.go) on an image that worked
// before.
func holdLane(interactive bool, toolApprovals, taskMode string) bool {
	return !interactive && toolApprovals == "hold" && taskMode != "exec"
}

// holdLaneHarnessCredentials is stampHarnessCredential for a run on the hold
// lane alone: only that lane launches a harness through wardyn-toolgate, so no
// other run is minted any. Its proxy then carries no digest, and any tool-gate
// call from it is decided as a call from no harness is.
func (s *Server) holdLaneHarnessCredentials(ctx context.Context, run types.AgentRun, p dispatchParams) ([]types.HarnessCredential, []runner.ManagedFile, error) {
	if !p.holdLane() {
		return nil, nil, nil
	}
	return s.stampHarnessCredential(ctx, run, p.HarnessToolRules, p.Drives)
}

// holdLane is holdLane for this dispatch.
func (p dispatchParams) holdLane() bool {
	return holdLane(p.Interactive, p.ToolApprovals, p.TaskMode)
}

// agentPolicyBasis names what a run's managed settings were generated from,
// for a failure hint or a 201 warning: the hold lane when it, not the level,
// chose the document.
func agentPolicyBasis(level types.AutonomyLevel, hold, locked bool) string {
	switch {
	case hold && agentpolicy.HoldTakesOver(level, locked):
		return "tool approvals on hold"
	case level == "" || level == types.AutonomyL3:
		return "tool rules" // the level brings no document, so only an interactive run's rules do
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
//
// locked is the governance rubric's agent_guardrail_locks term, frozen on the
// dispatch ceiling.
//
// The document follows the harness set, not only the started agent: a harness
// the set carries but Wardyn does not start (one a person starts by hand, or a
// command starts) runs under the same file as one Wardyn starts.
//
// attended is attendedToolRules: in an interactive run Claude Code's managed
// settings are what enforce them, so they are written into the document (into
// the attended base where the level brings none). A run whose rules cannot be
// written exactly, or whose runner cannot deliver the document, is refused
// (agentPolicyRefusal) rather than launched with its rules unenforced.
func (s *Server) agentPolicyFor(ctx context.Context, run types.AgentRun, harnesses []types.RunHarness, hold, locked bool, attended []types.ToolRule) (runAgentPolicy, error) {
	path, content, ok := agentpolicy.ForAgent(run.Agent, run.AutonomyLevel, hold, locked)
	for _, h := range harnesses {
		if !ok {
			path, content, ok = agentpolicy.ForAgent(h.Harness, run.AutonomyLevel, hold, locked)
		}
	}
	p := runAgentPolicy{path: path, hold: hold, locked: agentpolicy.IsLocked(content)}
	if run.Agent == claudeCodeHarness || slices.ContainsFunc(harnesses, func(h types.RunHarness) bool { return h.Harness == claudeCodeHarness }) {
		withRules, err := agentpolicy.WithToolRules(content, attended)
		if err != nil {
			return runAgentPolicy{}, agentPolicyRefusal("This run was not launched: its tool rules could not be written into Claude Code's managed settings: " + err.Error())
		}
		if !bytes.Equal(withRules, content) {
			p.path, content, ok = agentpolicy.ClaudeCodeManagedSettingsPath, withRules, true
			p.toolRules = distinctToolNames(attended)
		}
	}
	if !ok {
		return runAgentPolicy{}, nil
	}
	p.bytes = len(content)
	caps, err := s.runCapabilities(ctx, run)
	if err != nil {
		return runAgentPolicy{}, fmt.Errorf("the runner's capabilities could not be read, so whether it can deliver this run's managed settings (%s) is unknown: %w",
			agentPolicyBasis(run.AutonomyLevel, hold, locked), err)
	}
	var deliverable bool
	p.withheld, deliverable = managedFilesGap(caps, run.ConfinementClass)
	if p.toolRules > 0 && (!deliverable || p.withheld != "") {
		// Withheld covers krun too: the file is placed there, but a guest that
		// runs as root could rewrite the rules it carries.
		return runAgentPolicy{}, agentPolicyRefusal("This run was not launched: Claude Code's managed settings enforce its tool rules in an interactive run, and this runner cannot vouch for them: " + p.withheld)
	}
	if !deliverable {
		return p, nil
	}
	p.files = []runner.ManagedFile{{Path: p.path, Content: content}}
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
// same fact the run.agent_policy.write row records, said where the person launching
// the run reads it. None when the run generates no file or the row will say
// delivered, and none for an exec run, which has no agent process to say it
// about. A Capabilities error says nothing here: dispatch fails that run with
// the reason.
//
// req is read once the gate may have derived its hold, and through
// requestIsInteractive: preflight never runs the no-task coercion, so the raw
// field would disagree with the launch about which lane a task-less run takes.
func (s *Server) managedSettingsUndeliveredWarning(ctx context.Context, req *createRunRequest, level types.AutonomyLevel, locked bool, class types.ConfinementClass) []string {
	hold := holdLane(requestIsInteractive(*req), req.ToolApprovals, req.TaskMode)
	if _, _, ok := agentpolicy.ForAgent(req.Agent, level, hold, locked); !ok || s.cfg.Runner == nil || req.TaskMode == "exec" {
		return nil
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return nil
	}
	reason, _ := managedFilesGap(caps, class)
	if reason == "" {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s's managed settings for %s are not delivered: %s, so this run's agent runs under its launch flags alone and a repository's own settings can let it run tools without asking",
		autonomyAgentLabel(req.Agent), agentPolicyBasis(level, hold, locked), reason)}
}

// auditAgentPolicy records run.agent_policy.write once the agent's container exists,
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
		// Enough to tell the documents apart in a trail (L0 and L1 share their
		// bytes; `level` separates them).
		"bytes":     p.bytes,
		"delivered": delivered,
	}
	if p.locked {
		data["variant"] = "l2_locked"
	}
	if p.toolRules > 0 {
		// On every interactive run whose tool rules the document carries: what
		// says why a run with no level, or L3, has one.
		data["tool_rules"] = p.toolRules
	}
	if p.hold {
		// On every hold-lane run: what says why a run with no level, or an
		// L2/L3 one, got the gated document.
		data["tool_approvals"] = "hold"
	}
	if !delivered {
		reason := p.withheld
		if reason == "" {
			reason = "the sandbox spec did not carry the file"
		}
		data["reason"] = reason
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.agent_policy.write",
		run.ID.String(), "success", mustJSON(data)))
}

// distinctToolNames counts the tools rules name, "*" aside.
func distinctToolNames(rules []types.ToolRule) int {
	var names []string
	for _, r := range rules {
		if r.Tool != "*" {
			names = append(names, r.Tool)
		}
	}
	slices.Sort(names)
	return len(slices.Compact(names))
}
