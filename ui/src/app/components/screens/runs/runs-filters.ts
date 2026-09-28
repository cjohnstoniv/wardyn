/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3/L4 — "every filter is in the URL" (design.md §1). Pure parse/
// serialize so Back/Forward and a shared link both just work, and so the
// rule is one small vitest-covered function instead of five scattered
// useSearchParams reads.
//
// L4 added `scope` (H-4: Everyone/Mine, Admin view only — the URL param is
// `owner=me`, the SAME name §3.1's server-side filter already answers to, so
// a shared link and the wire request are the one string) and `group` (H-6:
// Group by Workspace/Title, a display-only option that never changes what the
// server returns — see runs-groups.ts).
import type { RunsEndedWithin, RunsListStatus } from "../../../lib/api/runs";

export type RunsStatusFilter = "all" | RunsListStatus;
export type RunsWhoseRuns = "all" | "mine";
export type RunsGroupBy = "sections" | "workspace" | "title";

export interface RunsFilterState {
  q: string;
  status: RunsStatusFilter;
  endedWithin: RunsEndedWithin;
  includeKilled: boolean;
  workspace: string; // "all" or a workspace/repo value
  // Admin view only (H-4). Meaningless in the User view, which always forces
  // owner=me server-side regardless of what this reads.
  scope: RunsWhoseRuns;
  group: RunsGroupBy;
}

// H-2: ended runs show 7 days by default; killed runs show 24h (includeKilled
// off) regardless of the window. H-4: Everyone by default. H-6: Sections.
export const DEFAULT_RUNS_FILTERS: RunsFilterState = {
  q: "",
  status: "all",
  endedWithin: "7d",
  includeKilled: false,
  workspace: "all",
  scope: "all",
  group: "sections",
};

const STATUSES: ReadonlySet<string> = new Set(["all", "needs", "active", "ended", "failed", "killed"]);
const WINDOWS: ReadonlySet<string> = new Set(["24h", "7d", "30d", "all"]);
const GROUPS: ReadonlySet<string> = new Set(["sections", "workspace", "title"]);

export function parseRunsFilters(params: URLSearchParams): RunsFilterState {
  const status = params.get("status") ?? DEFAULT_RUNS_FILTERS.status;
  const endedWithin = params.get("ended_within") ?? DEFAULT_RUNS_FILTERS.endedWithin;
  const group = params.get("group") ?? DEFAULT_RUNS_FILTERS.group;
  return {
    q: params.get("q") ?? "",
    status: STATUSES.has(status) ? (status as RunsStatusFilter) : "all",
    endedWithin: WINDOWS.has(endedWithin) ? (endedWithin as RunsEndedWithin) : "7d",
    includeKilled: params.get("include_killed") === "1",
    workspace: params.get("workspace") ?? "all",
    scope: params.get("owner") === "me" ? "mine" : "all",
    group: GROUPS.has(group) ? (group as RunsGroupBy) : "sections",
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
  if (f.scope === "mine") params.set("owner", "me");
  if (f.group !== DEFAULT_RUNS_FILTERS.group) params.set("group", f.group);
  return params;
}

// group is deliberately excluded: it never changes what is fetched (or
// whether the fetch came back empty), only how the same rows are laid out —
// see design.md §6 H-6. scope IS included: Mine narrows the server read, so a
// deployment with other people's runs but none of the viewer's own reads as
// "no match", never as first-run or quiet.
export function runsFiltersAreDefault(f: RunsFilterState): boolean {
  return (
    f.q === "" &&
    f.status === DEFAULT_RUNS_FILTERS.status &&
    f.endedWithin === DEFAULT_RUNS_FILTERS.endedWithin &&
    !f.includeKilled &&
    f.workspace === DEFAULT_RUNS_FILTERS.workspace &&
    f.scope === DEFAULT_RUNS_FILTERS.scope
  );
}

// The server request this filter state resolves to — status="all" sends no
// status param at all (parseRunsListParams' own "no filter" reading).
export function runsFilterToServerStatus(status: RunsStatusFilter): RunsListStatus | undefined {
  return status === "all" ? undefined : status;
}

// RunsListFilter.owner's own doc: never send "me"/"all" speculatively, since
// that would short-circuit the server's fail-closed owner force for a member.
// "all" (Everyone, the Admin view's own default) omits the param entirely, so
// only an explicit Mine ever asks for anything.
export function runsFilterToServerOwner(scope: RunsWhoseRuns): "me" | undefined {
  return scope === "mine" ? "me" : undefined;
}

// Picking a saved view is a plain navigation to its OWN query string (a
// saved view is a complete state, not a merge onto the current filters) —
// except for Everyone/Mine (H-4), which is a fact about WHO is asking, not
// something a view remembers: none of the built-ins carry an `owner` param,
// and even a custom view saved under Mine must not silently flip the caller
// back to Mine on every pick. This drops whatever `owner` the view's own
// search carries and reapplies the CURRENT scope in its place (Admin view
// only — the User view never sends `owner` at all).
export function applySavedViewOwner(
  viewSearch: string,
  adminView: boolean,
  currentScope: RunsWhoseRuns,
): string {
  const params = new URLSearchParams(viewSearch);
  params.delete("owner");
  if (adminView && currentScope === "mine") params.set("owner", "me");
  return params.toString();
}

function withoutOwner(search: string): string {
  const params = new URLSearchParams(search);
  params.delete("owner");
  return params.toString();
}

// The Saved view select's own "which option is this" fact. `applySavedViewOwner`
// above keeps the CURRENT Everyone/Mine across a pick, so the URL right after
// picking "Failed this week" under Mine is the view's own search PLUS
// `owner=me` — comparing that verbatim against the view's stored search
// (which never carries `owner`) always misses, reading as Custom even though
// a real view IS selected. In the Admin view, `owner` is compared away on
// both sides; the User view never carries it, so an exact match there is
// already the same comparison.
export function matchesSavedView(viewSearch: string, currentSearch: string, adminView: boolean): boolean {
  if (!adminView) return viewSearch === currentSearch;
  return withoutOwner(viewSearch) === withoutOwner(currentSearch);
}
