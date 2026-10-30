/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R17-N3 — the Runs board's segments. Every run is in exactly one, read from the shape the run was
// set up from (AgentRun.shape). A run with `service` is a Service, whatever its shape.
import type { AgentRun } from "../../../lib/types";

export type RunSegment = "agent_tasks" | "environments" | "apps" | "services";
export type RunsSegmentFilter = "all" | RunSegment;

export const RUN_SEGMENTS: readonly RunSegment[] = ["agent_tasks", "environments", "apps", "services"];

// The one place a run is put in a segment. A run with no stored shape (older, or one that fit more
// than one shape) is placed by the fallback rule: an agent harness is an Agent task; otherwise an
// interactive run is an Environment; otherwise it is Apps & scripts.
export function runSegment(run: AgentRun): RunSegment {
  if (run.service) return "services";
  switch (run.shape) {
    case "agent_task":
      return "agent_tasks";
    case "environment":
      return "environments";
    case "application":
      return "apps";
  }
  if (run.agent) return "agent_tasks";
  return run.experience === "interactive" || run.interactive ? "environments" : "apps";
}

export function segmentCounts(runs: AgentRun[]): Record<RunsSegmentFilter, number> {
  const counts = { all: runs.length, agent_tasks: 0, environments: 0, apps: 0, services: 0 };
  for (const r of runs) counts[runSegment(r)]++;
  return counts;
}

export function filterBySegment(runs: AgentRun[], segment: RunsSegmentFilter): AgentRun[] {
  return segment === "all" ? runs : runs.filter((r) => runSegment(r) === segment);
}
