/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L4 — H-6: "Group by Workspace or Title is a display option saved in
// the URL." Pure regrouping of the non-attention rows (runs-model.ts's
// nonAttentionRuns — Needs you / Waiting on the owner keep their own fixed
// sections regardless of this option, design.md §6's own mock). No JSX, so
// it is cheap to exhaustively vitest, same reason runs-model.ts has none.
import type { AgentRun } from "../../../lib/types";
import { repoLabel, rowHeadline } from "./board-groups";
import type { RunsGroupBy } from "./runs-filters";

export interface RunGroup {
  label: string;
  runs: AgentRun[];
}

function keyFor(by: "workspace" | "title"): (run: AgentRun) => string {
  return by === "workspace" ? (r) => repoLabel(r).text : rowHeadline;
}

// Groups by first appearance (not sorted alphabetically) — the incoming list
// is already the server's own order (needs first, then live, then ended by
// recency; design.md §3.1), so the group that contains the most recently
// active run keeps sorting first.
export function groupRunsBy(runs: readonly AgentRun[], by: Exclude<RunsGroupBy, "sections">): RunGroup[] {
  const key = keyFor(by);
  const order: string[] = [];
  const byKey = new Map<string, AgentRun[]>();
  for (const run of runs) {
    const k = key(run);
    if (!byKey.has(k)) {
      order.push(k);
      byKey.set(k, []);
    }
    byKey.get(k)!.push(run);
  }
  return order.map((label) => ({ label, runs: byKey.get(label)! }));
}
