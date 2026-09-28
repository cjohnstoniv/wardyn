/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3 — the D2 filter bar (design.md §1/§2.1), the L3 subset: search,
// Status, Ended within, Workspace, Include killed. Whose-runs (Everyone/
// Mine), saved views and Group-by are L4 (DO NOT TOUCH) — this bar's own
// state shape (runs-filters.ts) already has room for them without a rework.
import { Search } from "lucide-react";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { RUNS_FILTERS, RUNS_INCLUDE_KILLED } from "../../wardyn/copy/runs-landing";
import type { RunsFilterState, RunsStatusFilter } from "./runs-filters";
import type { RunsEndedWithin } from "../../../lib/api/runs";

const WORKSPACE_ALL = "all";

export function RunsFilterBar({
  filters,
  workspaces,
  onChange,
}: {
  filters: RunsFilterState;
  /** Workspace options — derived from the currently loaded page, same
   *  ponytail bound board-groups.ts's old titleGroups carried: a workspace
   *  split across a pagination boundary is a later refinement. */
  workspaces: string[];
  onChange: (next: RunsFilterState) => void;
}) {
  return (
    <div className="mt-3.5 flex flex-wrap items-center gap-2" role="search">
      <div className="relative w-full max-w-xs">
        <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          aria-label="Search runs"
          placeholder={RUNS_FILTERS.SEARCH_PLACEHOLDER}
          value={filters.q}
          onChange={(e) => onChange({ ...filters, q: e.target.value })}
          className="pl-9"
        />
      </div>

      <Select
        value={filters.status}
        onValueChange={(v) => onChange({ ...filters, status: v as RunsStatusFilter })}
      >
        <SelectTrigger size="sm" className="w-[160px]" aria-label={RUNS_FILTERS.STATUS_LABEL}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="all">{RUNS_FILTERS.STATUS_ALL}</SelectItem>
          <SelectItem value="needs">{RUNS_FILTERS.STATUS_NEEDS}</SelectItem>
          <SelectItem value="active">{RUNS_FILTERS.STATUS_ACTIVE}</SelectItem>
          <SelectItem value="ended">{RUNS_FILTERS.STATUS_ENDED}</SelectItem>
          <SelectItem value="failed">{RUNS_FILTERS.STATUS_FAILED}</SelectItem>
          <SelectItem value="killed">{RUNS_FILTERS.STATUS_KILLED}</SelectItem>
        </SelectContent>
      </Select>

      <Select
        value={filters.endedWithin}
        onValueChange={(v) => onChange({ ...filters, endedWithin: v as RunsEndedWithin })}
      >
        <SelectTrigger size="sm" className="w-[160px]" aria-label={RUNS_FILTERS.ENDED_WITHIN_LABEL}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="24h">{RUNS_FILTERS.ENDED_24H}</SelectItem>
          <SelectItem value="7d">{RUNS_FILTERS.ENDED_7D}</SelectItem>
          <SelectItem value="30d">{RUNS_FILTERS.ENDED_30D}</SelectItem>
          <SelectItem value="all">{RUNS_FILTERS.ENDED_ALL}</SelectItem>
        </SelectContent>
      </Select>

      <Select
        value={filters.workspace}
        onValueChange={(v) => onChange({ ...filters, workspace: v })}
      >
        <SelectTrigger size="sm" className="w-[190px]" aria-label={RUNS_FILTERS.WORKSPACE_LABEL}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={WORKSPACE_ALL}>{RUNS_FILTERS.WORKSPACE_ALL}</SelectItem>
          {workspaces.map((w) => (
            <SelectItem key={w} value={w}>
              {w}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>

      <label className="flex items-center gap-1.5 text-meta text-muted-foreground">
        <input
          type="checkbox"
          checked={filters.includeKilled}
          onChange={(e) => onChange({ ...filters, includeKilled: e.target.checked })}
        />
        {RUNS_INCLUDE_KILLED}
      </label>
    </div>
  );
}
