// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Several agents in one background run (#1987, WARDYN_MULTI_AGENT_TASKS, off by default):
// workload.agent runs workload.task and every other included harness runs its own
// tools[].task, side by side in one sandbox. The sandbox has one policy, one proxy and
// one set of mounts, so nothing reaches further than one agent's run would; what can
// differ per agent is held to the A8 rule, every agent's rules met (strictest wins):
//
//   - tool rules: every agent's compiled set is the meet of all of them (meetHarnessSets),
//     and an agent that cannot enforce the met set is refused (multiAgentToolRulesRefusal);
//   - the gated lane, and so each agent's inner walls: the run goes on it when any agent
//     needs it, and every agent with the lane is held there;
//   - each door a single-agent background run of an agent meets (the governance profile's
//     codex refusal, autonomy L1, an inherited MCP hold) is met by every agent (taskAgents).
//
// The run ends when the last agent exits and fails if any failed (agent-run-set --tasks).

// hasOwnTask reports whether t carries its own task.
func hasOwnTask(t client.IncludedTool) bool { return t.Task != "" }

// isAgentTask reports whether req is a background agent task.
func isAgentTask(req createRunRequest) bool {
	return req.Experience == client.ExperienceBackground && req.Workload != nil && req.Workload.Kind == client.WorkloadAgentTask
}

// isMultiAgentTask reports whether req runs several agents side by side.
func isMultiAgentTask(req createRunRequest) bool {
	return isAgentTask(req) && slices.ContainsFunc(req.Tools, hasOwnTask)
}

// taskAgents is every agent a background run starts: the run's agent, then each
// included harness with its own task. A single-agent run's is its agent alone, so a
// door that reads it reads exactly what it read before.
func taskAgents(req createRunRequest) []string {
	agents := []string{req.Agent}
	if !isMultiAgentTask(req) {
		return agents
	}
	for _, t := range req.Tools {
		if hasOwnTask(t) {
			agents = append(agents, t.ID)
		}
	}
	return agents
}

// multiAgentTasksOffRefusal refuses a request that gives an agent its own task while
// the switch is off. Every request without one is untouched.
func (s *Server) multiAgentTasksOffRefusal(req createRunRequest) *runRefusal {
	if s.cfg.MultiAgentTasks || !slices.ContainsFunc(req.Tools, hasOwnTask) {
		return nil
	}
	return runError(http.StatusUnprocessableEntity, reasonMultiAgentTasksOff,
		"Several agents in one background run are turned off on this server (WARDYN_MULTI_AGENT_TASKS). Launch one agent per run, or ask an admin to turn it on.")
}

// multiAgentTaskConflict holds a background agent task with several agents to one
// shape: workload.agent is one of tools and its task is workload.task, and every other
// harness carries its own task. nil when the shape holds.
func multiAgentTaskConflict(req createRunRequest) *runRefusal {
	conflict := func(format string, args ...any) *runRefusal {
		return runError(http.StatusUnprocessableEntity, reasonRunModeConflict, fmt.Sprintf(format, args...))
	}
	agent := req.Workload.Agent
	if !slices.ContainsFunc(req.Tools, func(t client.IncludedTool) bool { return t.ID == agent && t.Kind == client.IncludedToolHarness }) {
		return conflict("With several agents, workload.agent names one of tools: the harness whose task is workload.task. %s is not included.", agent)
	}
	for i, t := range req.Tools {
		switch {
		case t.Kind != client.IncludedToolHarness:
		case t.ID == agent && hasOwnTask(t):
			return conflict("tools[%d].task: %s is workload.agent, and its task is workload.task. Give it once.", i, t.ID)
		case t.ID == noHarnessCLI && hasOwnTask(t):
			return conflict("tools[%d].task: Your own tools runs no agent, so it takes no task.", i)
		case t.ID != agent && t.ID != noHarnessCLI && !hasOwnTask(t):
			return conflict("tools[%d].task: give %s its own task. In a background run with several agents every harness but workload.agent carries one.", i, t.ID)
		}
	}
	return multiAgentToolRulesRefusal(req)
}

// toolTaskOutsideAgentTaskMsg refuses tools[i].task anywhere but a background agent task.
func toolTaskOutsideAgentTaskMsg(i int) string {
	return fmt.Sprintf("tools[%d].task: only a background agent task gives each agent its own task.", i)
}

// meetHarnessSets is the A8 rule over per-harness sets: every harness gets the meet of
// all of them, each tool any set names at the strictest effect any set gives it (a set
// that does not name it gives its default, else laneDefault), and "*" the strictest
// default. Adding an agent can only narrow a rule, never widen one.
func meetHarnessSets(sets []types.HarnessToolRules, laneDefault types.ToolEffect) []types.HarnessToolRules {
	if len(sets) < 2 {
		return sets
	}
	effectIn := func(set types.HarnessToolRules, tool string) types.ToolEffect {
		if i := slices.IndexFunc(set.Rules, func(r types.ToolRule) bool { return r.Tool == tool }); i >= 0 {
			return set.Rules[i].Effect
		}
		if set.Default != "" {
			return set.Default
		}
		return laneDefault
	}
	var met types.HarnessToolRules
	for _, set := range sets {
		if set.Default != "" {
			met.Default = stricterToolEffect(met.Default, set.Default)
		}
		for _, r := range set.Rules {
			if slices.ContainsFunc(met.Rules, func(m types.ToolRule) bool { return m.Tool == r.Tool }) {
				continue
			}
			eff := types.ToolEffect("")
			for _, other := range sets {
				eff = stricterToolEffect(eff, effectIn(other, r.Tool))
			}
			met.Rules = append(met.Rules, types.ToolRule{Tool: r.Tool, Effect: eff})
		}
	}
	if met.Default != "" && slices.ContainsFunc(sets, func(set types.HarnessToolRules) bool { return set.Default == "" }) {
		met.Default = stricterToolEffect(met.Default, laneDefault)
	}
	out := make([]types.HarnessToolRules, len(sets))
	for i, set := range sets {
		out[i] = types.HarnessToolRules{Harness: set.Harness, Rules: slices.Clone(met.Rules), Default: met.Default}
	}
	return out
}

// multiAgentToolRulesRefusal refuses a run with several agents in which an agent cannot
// enforce the rules the people's own are met into: each harness is judged with every
// agent's own rules and defaults met as if they were its own (harnessToolRulesRefusal),
// so a hold or a built-in rule another agent brings refuses a harness with no gate, as
// that rule refuses it on its own run. The policy's rules are judged as on a
// single-agent run.
func multiAgentToolRulesRefusal(req createRunRequest) *runRefusal {
	if !isMultiAgentTask(req) {
		return nil
	}
	var met client.IncludedTool
	for _, t := range req.Tools {
		if t.Kind != client.IncludedToolHarness {
			continue
		}
		met.DefaultEffect = stricterToolEffect(met.DefaultEffect, t.DefaultEffect)
		for _, r := range t.ToolRules {
			if !slices.ContainsFunc(met.ToolRules, func(m types.ToolRule) bool { return m.Tool == r.Tool }) {
				met.ToolRules = append(met.ToolRules, types.ToolRule{Tool: r.Tool})
			}
		}
	}
	for i, r := range met.ToolRules {
		for _, t := range req.Tools {
			mine := t.DefaultEffect
			if j := slices.IndexFunc(t.ToolRules, func(o types.ToolRule) bool { return o.Tool == r.Tool }); j >= 0 {
				mine = t.ToolRules[j].Effect
			}
			met.ToolRules[i].Effect = stricterToolEffect(met.ToolRules[i].Effect, mine)
		}
	}
	for _, t := range req.Tools {
		if t.Kind != client.IncludedToolHarness || t.ID == noHarnessCLI {
			continue
		}
		as := met
		as.ID, as.Kind = t.ID, t.Kind
		if refusal := harnessToolRulesRefusal(as); refusal != nil {
			return refusal.forTool(t.ID)
		}
	}
	return nil
}

// agentTaskArgv is the task exec's argv: the run's agent on the run's task, or, when any
// harness carries its own task, the harnesses image's supervisor over every agent
// (agent-run-set --tasks), the run's agent first.
func agentTaskArgv(run types.AgentRun, harnesses []types.RunHarness) []string {
	if !slices.ContainsFunc(harnesses, func(h types.RunHarness) bool { return h.Task != "" }) {
		return []string{"/usr/local/bin/agent-run", run.Task}
	}
	argv := []string{"/usr/local/bin/agent-run", "--tasks", run.Agent, run.Task}
	for _, h := range harnesses {
		if h.Task != "" {
			argv = append(argv, h.Harness, h.Task)
		}
	}
	return argv
}

// startedAgents is every agent a run's task exec starts: the run's agent, then each
// harness on a task of its own that has Wardyn's approval lane, the ones that read a
// toolgate credential.
func startedAgents(run types.AgentRun, harnesses []types.RunHarness) []string {
	agents := []string{run.Agent}
	for _, h := range harnesses {
		if h.Task != "" && agentHasHoldLane(h.Harness) {
			agents = append(agents, h.Harness)
		}
	}
	return agents
}

// runAgentState is one agent of a run with several agents, on GET /runs/{id}/output:
// running, or exited with its code. client.RunAgentState mirrors it.
type runAgentState struct {
	Agent    string `json:"agent"`
	State    string `json:"state"` // "running" or "exited"
	ExitCode *int   `json:"exit_code,omitempty"`
}

// agentsStatusPrefix starts the supervisor's status line (agent-run-set --tasks). Every
// line an agent prints is prefixed with its id, so an unprefixed one is the supervisor's.
const agentsStatusPrefix = "wardyn-agents: "

// writeRunOutput writes GET /runs/{id}/output's body: with ?agent=<id>, that agent's
// lines alone, their prefix dropped, and on a run with several agents each one's state.
func (s *Server) writeRunOutput(w http.ResponseWriter, r *http.Request, runID uuid.UUID, resp runOutputResponse) {
	resp.Agents = s.runOutputAgents(r.Context(), runID, resp.Output)
	if agent := r.URL.Query().Get("agent"); agent != "" {
		var lines []string
		for _, l := range strings.Split(resp.Output, "\n") {
			if rest, ok := strings.CutPrefix(l, "["+agent+"] "); ok {
				lines = append(lines, rest)
			}
		}
		resp.Output = strings.Join(lines, "\n")
	}
	writeJSON(w, http.StatusOK, resp)
}

// runOutputAgents reads each agent's state off the last status line in output. Only a
// run whose harness rows give an agent its own task has one, and only its agents are
// read, so a single-agent run that prints such a line itself is shown no states.
func (s *Server) runOutputAgents(ctx context.Context, runID uuid.UUID, output string) []runAgentState {
	i := strings.LastIndex("\n"+output, "\n"+agentsStatusPrefix)
	if i < 0 {
		return nil
	}
	line, _, _ := strings.Cut(output[i+len(agentsStatusPrefix):], "\n")
	st, ok := s.cfg.Store.(store.RunHarnessStore)
	if !ok {
		return nil
	}
	rows, err := st.ListRunHarnesses(ctx, runID)
	if err != nil || !slices.ContainsFunc(rows, func(h types.RunHarness) bool { return h.Task != "" }) {
		return nil
	}
	var out []runAgentState
	for _, field := range strings.Fields(line) {
		agent, state, _ := strings.Cut(field, "=")
		if !slices.ContainsFunc(rows, func(h types.RunHarness) bool { return h.Harness == agent && (h.Task != "" || h.Starts) }) {
			continue
		}
		a := runAgentState{Agent: agent, State: "running"}
		if code, exited := strings.CutPrefix(state, "exited="); exited {
			if n, err := strconv.Atoi(code); err == nil {
				a.State, a.ExitCode = "exited", &n
			}
		}
		out = append(out, a)
	}
	return out
}

// runDetailAgent is one agent of a run with several agents, on GET /runs/{id}: its id
// and its own task. RunDetailAgent in ui/src/app/lib/types/runs.ts mirrors it.
type runDetailAgent struct {
	Agent string `json:"agent"`
	Task  string `json:"task,omitempty"`
}

// runDetailAgents is the run's agent on the run's task, then each harness on a task of
// its own, in the order the run declared them. nil for a run with no such harness, so
// every other run's GET /runs/{id} body is what it was.
func (s *Server) runDetailAgents(ctx context.Context, run types.AgentRun) []runDetailAgent {
	st, ok := s.cfg.Store.(store.RunHarnessStore)
	if !ok {
		return nil
	}
	rows, err := st.ListRunHarnesses(ctx, run.ID)
	if err != nil || !slices.ContainsFunc(rows, func(h types.RunHarness) bool { return h.Task != "" }) {
		return nil
	}
	out := []runDetailAgent{{Agent: run.Agent, Task: run.Task}}
	for _, h := range rows {
		if h.Task != "" {
			out = append(out, runDetailAgent{Agent: h.Harness, Task: h.Task})
		}
	}
	return out
}
