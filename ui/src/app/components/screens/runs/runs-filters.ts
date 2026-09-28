/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3 — "every filter is in the URL" (design.md §1). Pure parse/
// serialize so Back/Forward and a shared link both just work, and so the
// rule is one small vitest-covered function instead of five scattered
// useSearchParams reads.
//
// L3 scope only: q / status / endedWithin / includeKilled / workspace.
// Whose-runs (Everyone/Mine), saved views and Group-by are L4 (design.md §6
// build plan; DO NOT TOUCH) — their own URL params can be added there without
// touching this shape.
import type { RunsEndedWithin, RunsListStatus } from "../../../lib/api/runs";

export type RunsStatusFilter = "all" | RunsListStatus;

export interface RunsFilterState {
  q: string;
  status: RunsStatusFilter;
  endedWithin: RunsEndedWithin;
  includeKilled: boolean;
  workspace: string; // "all" or a workspace/repo value
}

// H-2: ended runs show 7 days by default; killed runs show 24h (includeKilled
// off) regardless of the window.
export const DEFAULT_RUNS_FILTERS: RunsFilterState = {
  q: "",
  status: "all",
  endedWithin: "7d",
  includeKilled: false,
  workspace: "all",
};

const STATUSES: ReadonlySet<string> = new Set(["all", "needs", "active", "ended", "failed", "killed"]);
const WINDOWS: ReadonlySet<string> = new Set(["24h", "7d", "30d", "all"]);

export function parseRunsFilters(params: URLSearchParams): RunsFilterState {
  const status = params.get("status") ?? DEFAULT_RUNS_FILTERS.status;
  const endedWithin = params.get("ended_within") ?? DEFAULT_RUNS_FILTERS.endedWithin;
  return {
    q: params.get("q") ?? "",
    status: STATUSES.has(status) ? (status as RunsStatusFilter) : "all",
    endedWithin: WINDOWS.has(endedWithin) ? (endedWithin as RunsEndedWithin) : "7d",
    includeKilled: params.get("include_killed") === "1",
    workspace: params.get("workspace") ?? "all",
  };
}

// Serializes ONLY the non-default fields, so a Default view's URL is clean
// (no `?status=all&ended_within=7d&...` noise) and a shared link round-trips
// through parseRunsFilters back to the same state either way.
export function serializeRunsFilters(f: RunsFilterState): URLSearchParams {
  const params = new URLSearchParams();
  if (f.q) params.set("q", f.q);
  if (f.status !== DEFAULT_RUNS_FILTERS.status) params.set("status", f.status);
  if (f.endedWithin !== DEFAULT_RUNS_FILTERS.endedWithin) params.set("ended_within", f.endedWithin);
  if (f.includeKilled) params.set("include_killed", "1");
  if (f.workspace !== DEFAULT_RUNS_FILTERS.workspace) params.set("workspace", f.workspace);
  return params;
}

export function runsFiltersAreDefault(f: RunsFilterState): boolean {
  return (
    f.q === "" &&
    f.status === DEFAULT_RUNS_FILTERS.status &&
    f.endedWithin === DEFAULT_RUNS_FILTERS.endedWithin &&
    !f.includeKilled &&
    f.workspace === DEFAULT_RUNS_FILTERS.workspace
  );
}

// The server request this filter state resolves to — status="all" sends no
// status param at all (parseRunsListParams' own "no filter" reading).
export function runsFilterToServerStatus(status: RunsStatusFilter): RunsListStatus | undefined {
  return status === "all" ? undefined : status;
}
