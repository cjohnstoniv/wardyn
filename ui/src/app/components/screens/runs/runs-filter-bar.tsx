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
  RUNS_SAVE_VIEW_CANCEL,
  RUNS_SAVE_VIEW_DEFAULT_NAME,
  RUNS_SAVE_VIEW_NAME_LABEL,
  RUNS_SAVE_VIEW_SUBMIT,
  RUNS_SAVED_VIEW_CUSTOM,
  runsSavedViewOption,
} from "../../wardyn/copy/runs-landing";
import { matchesSavedView, type RunsFilterState, type RunsGroupBy, type RunsStatusFilter, type RunsWhoseRuns } from "./runs-filters";
import type { RunsEndedWithin } from "../../../lib/api/runs";
import type { SavedRunsView } from "./runs-saved-views";

const WORKSPACE_ALL = "all";
// Never a real saved view's index — it is only ever the sentinel item shown
// when the current URL matches none of them (mock's "View · Custom"). Must
// be non-empty: a Radix SelectItem's value may never be "".
const CUSTOM_VIEW_VALUE = "custom";

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
  // The box is typed into a local draft: the URL answers a keystroke a render
  // late, and a value read back from it drops the characters typed meanwhile.
  const [draft, setDraft] = React.useState(filters.q);
  // What this box has sent and the URL has not echoed; a q that is none of
  // those came from elsewhere (a saved view, Clear) and wins.
  const pending = React.useRef<string[]>([]);
  React.useEffect(() => {
    const i = pending.current.indexOf(filters.q);
    if (i >= 0) {
      pending.current = pending.current.slice(i + 1);
      return;
    }
    pending.current = [];
    setDraft(filters.q);
  }, [filters.q]);
  const [name, setName] = React.useState(RUNS_SAVE_VIEW_DEFAULT_NAME);
  // Indexed by position, not by its own search string — "Default" is itself
  // the empty query string, and a Radix SelectItem's value may never be "".
  // matchesSavedView ignores `owner` in the Admin view: applySavedViewOwner
  // (runs.tsx) reapplies the CURRENT Everyone/Mine onto a picked view's own
  // search, so the URL right after a pick carries `owner=me` under Mine even
  // though no built-in view ever stores that param.
  const selectedIndex = savedViews.findIndex((v) => matchesSavedView(v.search, currentSearch, adminView));
  const selectedView = selectedIndex >= 0 ? String(selectedIndex) : CUSTOM_VIEW_VALUE;

  return (
    <div className="mt-3.5 flex flex-wrap items-center gap-2" role="search">
      <div className="relative w-full max-w-xs">
        <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          aria-label="Search runs"
          placeholder={RUNS_FILTERS.SEARCH_PLACEHOLDER}
          value={draft}
          onChange={(e) => {
            // Typing back to the URL's own value is a no-op send: a router
            // that coalesces it with the keystrokes before it echoes only the
            // last one, so everything queued behind it can never be matched
            // and would swallow a later saved view that happens to equal one.
            pending.current = e.target.value === filters.q ? [] : [...pending.current, e.target.value];
            setDraft(e.target.value);
            onChange({ ...filters, q: e.target.value });
          }}
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
          if (v === CUSTOM_VIEW_VALUE) return; // informational only, mock (:615) offers no action for it
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
              {runsSavedViewOption(v.name)}
            </SelectItem>
          ))}
          {/* The current URL matches no saved view — every filter/search
              change leaves this true, so the trigger must never render
              blank (mock's own `st.savedIdx < 0` -> "View · Custom"). */}
          {selectedIndex < 0 && <SelectItem value={CUSTOM_VIEW_VALUE}>{RUNS_SAVED_VIEW_CUSTOM}</SelectItem>}
        </SelectContent>
      </Select>
      <Button type="button" size="sm" variant="outline" onClick={() => setSaving(true)}>
        {RUNS_SAVE_VIEW}
      </Button>

      {saving && (
        <form
          className="flex w-full flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            onSaveView(name.trim() || RUNS_SAVE_VIEW_DEFAULT_NAME);
            setName(RUNS_SAVE_VIEW_DEFAULT_NAME);
            setSaving(false);
          }}
        >
          <label htmlFor="runs-save-view-name" className="shrink-0 whitespace-nowrap text-meta text-muted-foreground">
            {RUNS_SAVE_VIEW_NAME_LABEL}
          </label>
          <Input
            id="runs-save-view-name"
            autoFocus
            maxLength={40}
            className="h-8 max-w-[220px]"
            value={name}
            onChange={(e) => setName(e.target.value)}
            onFocus={(e) => e.currentTarget.select()}
          />
          <Button type="submit" size="sm">
            {RUNS_SAVE_VIEW_SUBMIT}
          </Button>
          <Button type="button" size="sm" variant="ghost" onClick={() => setSaving(false)}>
            {RUNS_SAVE_VIEW_CANCEL}
          </Button>
        </form>
      )}
    </div>
  );
}
