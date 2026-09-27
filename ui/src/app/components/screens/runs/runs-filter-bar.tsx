/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3/L4 — the D2 filter bar (design.md §1/§2.1): search, Status, Ended
// within, Workspace, Group by, Whose runs (Admin view only), Include killed,
// Saved view, Save view.
import * as React from "react";
import { Search } from "lucide-react";
import { Input } from "../../ui/input";
import { Button } from "../../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import {
  RUNS_FILTERS,
  RUNS_INCLUDE_KILLED,
  RUNS_SAVE_VIEW,
  RUNS_SAVE_VIEW_NAME_LABEL,
} from "../../wardyn/copy/runs-landing";
import type { RunsFilterState, RunsGroupBy, RunsStatusFilter, RunsWhoseRuns } from "./runs-filters";
import type { RunsEndedWithin } from "../../../lib/api/runs";
import type { SavedRunsView } from "./runs-saved-views";

const WORKSPACE_ALL = "all";
// Never a real saved search — "" only ever means "matches no known view", so
// picking it back is a no-op rather than a broken navigation.
const CUSTOM_VIEW_VALUE = "";

export function RunsFilterBar({
  filters,
  workspaces,
  adminView,
  currentSearch,
  savedViews,
  onChange,
  onSelectView,
  onSaveView,
}: {
  filters: RunsFilterState;
  /** Workspace options — derived from the currently loaded page, same
   *  ponytail bound board-groups.ts's old titleGroups carried: a workspace
   *  split across a pagination boundary is a later refinement. */
  workspaces: string[];
  adminView: boolean;
  /** The current URL's query string (no leading "?") — matched against
   *  savedViews to decide which "Saved view" option (if any) is selected. */
  currentSearch: string;
  /** Built-in views followed by whatever this browser has saved (H-8). */
  savedViews: SavedRunsView[];
  onChange: (next: RunsFilterState) => void;
  /** Jumps straight to a saved view's own query string — a plain navigation,
   *  not a merge onto the current filters (a saved view is a complete state). */
  onSelectView: (search: string) => void;
  onSaveView: (name: string) => void;
}) {
  const [saving, setSaving] = React.useState(false);
  const [name, setName] = React.useState("");
  // Indexed by position, not by its own search string — "Default" is itself
  // the empty query string, and a Radix SelectItem's value may never be "".
  const selectedIndex = savedViews.findIndex((v) => v.search === currentSearch);
  const selectedView = selectedIndex >= 0 ? String(selectedIndex) : CUSTOM_VIEW_VALUE;

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

      <Select value={filters.group} onValueChange={(v) => onChange({ ...filters, group: v as RunsGroupBy })}>
        <SelectTrigger size="sm" className="w-[160px]" aria-label={RUNS_FILTERS.GROUP_LABEL}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="sections">{RUNS_FILTERS.GROUP_SECTIONS}</SelectItem>
          <SelectItem value="workspace">{RUNS_FILTERS.GROUP_WORKSPACE}</SelectItem>
          <SelectItem value="title">{RUNS_FILTERS.GROUP_TITLE}</SelectItem>
        </SelectContent>
      </Select>

      {/* H-4, Admin view only: the User view always shows the caller's own
          runs and has no control for it. */}
      {adminView && (
        <Select value={filters.scope} onValueChange={(v) => onChange({ ...filters, scope: v as RunsWhoseRuns })}>
          <SelectTrigger size="sm" className="w-[140px]" aria-label={RUNS_FILTERS.WHOSE_RUNS_LABEL}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{RUNS_FILTERS.EVERYONE}</SelectItem>
            <SelectItem value="mine">{RUNS_FILTERS.MINE}</SelectItem>
          </SelectContent>
        </Select>
      )}

      <label className="flex items-center gap-1.5 text-meta text-muted-foreground">
        <input
          type="checkbox"
          checked={filters.includeKilled}
          onChange={(e) => onChange({ ...filters, includeKilled: e.target.checked })}
        />
        {RUNS_INCLUDE_KILLED}
      </label>

      {/* H-8: saved views. Selecting one is a plain navigation to its own
          query string (a saved view is a complete state, not a merge). */}
      <Select
        value={selectedView}
        onValueChange={(v) => {
          const view = savedViews[Number(v)];
          if (view) onSelectView(view.search);
        }}
      >
        <SelectTrigger size="sm" className="w-[170px]" aria-label={RUNS_FILTERS.SAVED_VIEW_LABEL}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {savedViews.map((v, i) => (
            <SelectItem key={`${v.name}-${i}`} value={String(i)}>
              {v.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Button type="button" size="sm" variant="outline" onClick={() => setSaving(true)}>
        {RUNS_SAVE_VIEW}
      </Button>

      {saving && (
        <form
          className="flex w-full items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            onSaveView(name.trim() || "My view");
            setName("");
            setSaving(false);
          }}
        >
          <label htmlFor="runs-save-view-name" className="text-meta text-muted-foreground">
            {RUNS_SAVE_VIEW_NAME_LABEL}
          </label>
          <Input
            id="runs-save-view-name"
            autoFocus
            maxLength={40}
            className="h-8 max-w-[220px]"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <Button type="submit" size="sm">
            {RUNS_SAVE_VIEW}
          </Button>
          <Button type="button" size="sm" variant="ghost" onClick={() => setSaving(false)}>
            Cancel
          </Button>
        </form>
      )}
    </div>
  );
}
