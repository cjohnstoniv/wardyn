/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3041 — the phone filter bar's chips and its "Filters (n)" count. One entry
// per control that is off its default; search is not one (its box stays on
// screen), and neither is the Saved view select (it only reads "Custom"
// because some other control moved, so counting it would double-count).
// Removing a chip puts that one control back to its default.
import { RUNS_FILTERS, RUNS_INCLUDE_KILLED } from "../../wardyn/copy/runs-landing";
import { DEFAULT_RUNS_FILTERS, type RunsFilterState } from "./runs-filters";

export interface ActiveRunsFilter {
  key: "status" | "endedWithin" | "workspace" | "group" | "scope" | "includeKilled";
  label: string;
  /** The state with only this control reset. */
  without: RunsFilterState;
}

const STATUS_LABEL = {
  all: RUNS_FILTERS.STATUS_ALL,
  needs: RUNS_FILTERS.STATUS_NEEDS,
  active: RUNS_FILTERS.STATUS_ACTIVE,
  ended: RUNS_FILTERS.STATUS_ENDED,
  failed: RUNS_FILTERS.STATUS_FAILED,
  killed: RUNS_FILTERS.STATUS_KILLED,
} as const;
const ENDED_LABEL = {
  "24h": RUNS_FILTERS.ENDED_24H,
  "7d": RUNS_FILTERS.ENDED_7D,
  "30d": RUNS_FILTERS.ENDED_30D,
  all: RUNS_FILTERS.ENDED_ALL,
} as const;
const GROUP_LABEL = {
  sections: RUNS_FILTERS.GROUP_SECTIONS,
  workspace: RUNS_FILTERS.GROUP_WORKSPACE,
  title: RUNS_FILTERS.GROUP_TITLE,
} as const;

/** `adminView` false drops "Whose runs": the User view has no such control. */
export function activeRunsFilters(f: RunsFilterState, adminView: boolean): ActiveRunsFilter[] {
  const d = DEFAULT_RUNS_FILTERS;
  const out: ActiveRunsFilter[] = [];
  if (f.status !== d.status) out.push({ key: "status", label: STATUS_LABEL[f.status], without: { ...f, status: d.status } });
  if (f.endedWithin !== d.endedWithin)
    out.push({ key: "endedWithin", label: ENDED_LABEL[f.endedWithin], without: { ...f, endedWithin: d.endedWithin } });
  if (f.workspace !== d.workspace)
    out.push({ key: "workspace", label: RUNS_FILTERS.WORKSPACE_CHIP(f.workspace), without: { ...f, workspace: d.workspace } });
  if (f.group !== d.group) out.push({ key: "group", label: GROUP_LABEL[f.group], without: { ...f, group: d.group } });
  if (adminView && f.scope !== d.scope) out.push({ key: "scope", label: RUNS_FILTERS.MINE, without: { ...f, scope: d.scope } });
  if (f.includeKilled) out.push({ key: "includeKilled", label: RUNS_INCLUDE_KILLED, without: { ...f, includeKilled: false } });
  return out;
}
