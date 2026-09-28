/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L4 — H-8: "saved views live in localStorage, up to 10. 'Save view'
// names the current URL." A saved view is just a name plus a query string
// (never a pathname — the same saved view works from whichever route it was
// picked in, and a shared link never needs to cross the User/Admin boundary
// on its own). Built-ins are the same shape, computed from filter overrides
// so they always read `sections`/`7d`/Everyone wherever they weren't
// overridden, instead of drifting from DEFAULT_RUNS_FILTERS by hand.
import { lsGet, lsSet } from "../../../lib/storage";
import { DEFAULT_RUNS_FILTERS, serializeRunsFilters, type RunsFilterState } from "./runs-filters";

export interface SavedRunsView {
  name: string;
  search: string;
}

const STORAGE_KEY = "wardyn-runs-saved-views";
export const MAX_SAVED_RUNS_VIEWS = 10;

function searchFor(overrides: Partial<RunsFilterState>): string {
  return serializeRunsFilters({ ...DEFAULT_RUNS_FILTERS, ...overrides }).toString();
}

// design.md §2.1's own built-in names, mirroring the owner-approved packet's
// D2 view list (mock-08/home-runs-1197-packet.html).
export const BUILTIN_RUNS_VIEWS: readonly SavedRunsView[] = [
  { name: "Default", search: searchFor({}) },
  { name: "Failed this week", search: searchFor({ status: "failed" }) },
  { name: "Killed", search: searchFor({ status: "killed", endedWithin: "30d", includeKilled: true }) },
  { name: "By workspace", search: searchFor({ group: "workspace" }) },
];

function isSavedRunsView(v: unknown): v is SavedRunsView {
  return (
    !!v &&
    typeof v === "object" &&
    typeof (v as SavedRunsView).name === "string" &&
    typeof (v as SavedRunsView).search === "string"
  );
}

/** Reads the user-saved (non-built-in) views. A corrupted or hand-edited
 *  entry is dropped rather than crashing the page — this is convenience
 *  state, never a source of truth the rest of the app depends on. */
export function loadSavedRunsViews(): SavedRunsView[] {
  const raw = lsGet(STORAGE_KEY);
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter(isSavedRunsView) : [];
  } catch {
    return [];
  }
}

/** Saves `name` against the CURRENT url's search string (H-8). Oldest entry
 *  is dropped once the list would exceed MAX_SAVED_RUNS_VIEWS — "up to 10" as
 *  a rolling window, not a hard refusal the operator has to clear first. */
export function saveRunsView(name: string, search: string): SavedRunsView[] {
  const next = [...loadSavedRunsViews(), { name, search }];
  while (next.length > MAX_SAVED_RUNS_VIEWS) next.shift();
  lsSet(STORAGE_KEY, JSON.stringify(next));
  return next;
}
