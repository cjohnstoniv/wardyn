/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agents panel on a run with two or more agents: each agent's harness, its task and its own
// status, then the rule for how the run ends. The status words are the run states' own.
import type { RunDetailAgent, RunAgentState, RunState } from "../../../lib/types";
import { RUN_AGENTS } from "../../wardyn/copy";
import { AgentBadge, RunStateBadge } from "../../wardyn/primitives";
import { useAgentName } from "../../wardyn/use-agent-name";

/** An agent's status word as a run state: absent while it is unknown on a run that has ended. */
export function agentStatus(s: RunAgentState | undefined, live: boolean): RunState | undefined {
  if (!s || s.state === "running") return live ? "RUNNING" : undefined;
  return s.exit_code === 0 ? "COMPLETED" : "FAILED";
}

export function agentsSummary(statuses: readonly (RunState | undefined)[]): string | undefined {
  const n = statuses.length;
  const running = statuses.filter((s) => s === "RUNNING").length;
  const failed = statuses.filter((s) => s === "FAILED").length;
  const finished = failed + statuses.filter((s) => s === "COMPLETED").length;
  if (running > 0) return RUN_AGENTS.summaryRunning(n, running, finished);
  if (finished < n) return undefined;
  return failed > 0 ? RUN_AGENTS.summaryFailed(n, failed) : RUN_AGENTS.summaryAllFinished(n);
}

/** One agent's lines of the combined output: the supervisor prefixes each with `[agent] `; its own status lines have none. */
export function agentLines(output: string, agent: string): string {
  return output.split("\n").filter((l) => l.startsWith(`[${agent}] `)).join("\n");
}

export function AgentsPanel({
  agents,
  states,
  runAgent,
  runTask,
  live,
}: {
  agents: readonly RunDetailAgent[];
  /** From the output read; absent until the run has printed its first status line. */
  states?: readonly RunAgentState[];
  /** The run's own agent and task: that agent's task is the run's, not a harness row's. */
  runAgent: string;
  runTask: string;
  live: boolean;
}) {
  const name = useAgentName();
  const rows = agents.map((a) => ({ ...a, status: agentStatus(states?.find((s) => s.agent === a.agent), live), task: a.task ?? (a.agent === runAgent ? runTask : undefined) }));
  const summary = agentsSummary(rows.map((r) => r.status));
  return (
    <section aria-labelledby="run-agents-h" className="max-w-4xl rounded-xl border border-border bg-card">
      <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-3">
        <h2 id="run-agents-h" className="label-eyebrow">{RUN_AGENTS.title}</h2>
        {summary && <span className="text-xs text-muted-foreground">{summary}</span>}
      </div>
      <ul className="divide-y divide-border">
        {rows.map((r) => (
          <li key={r.agent} className="flex items-center gap-3 px-4 py-3">
            <AgentBadge agent={r.agent} withLabel={false} />
            <span className="flex min-w-0 flex-1 flex-col">
              <strong className="text-sm">{name(r.agent)}</strong>
              {r.task && <span className="truncate text-xs text-muted-foreground">{r.task}</span>}
            </span>
            {r.status && <RunStateBadge state={r.status} />}
          </li>
        ))}
      </ul>
      <p className="border-t border-border px-4 py-2.5 text-xs text-muted-foreground">{RUN_AGENTS.exitRule}</p>
    </section>
  );
}
