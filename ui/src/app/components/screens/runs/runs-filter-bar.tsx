/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3/L4 — the D2 filter bar (design.md §1/§2.1): search, Status, Ended
// within, Workspace, Group by, Whose runs (Admin view only), Include killed,
// Saved view, Save view. Under the `sm` breakpoint (#3041) only the search box
// stays on the row: a Filters (n) button opens a bottom sheet holding every
// other control, and the active filters show as removable chips beneath.
import * as React from "react";
import { Search, X } from "lucide-react";
import { Input } from "../../ui/input";
import { Button } from "../../ui/button";
import { Sheet, SheetClose, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from "../../ui/sheet";
import { Chip } from "../../wardyn/primitives";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import {
  RUNS_FILTERS,
  RUNS_FILTERS_SHEET,
  RUNS_INCLUDE_KILLED,
  RUNS_SAVE_VIEW,
  RUNS_SAVE_VIEW_CANCEL,
  RUNS_SAVE_VIEW_DEFAULT_NAME,
  RUNS_SAVE_VIEW_NAME_LABEL,
  RUNS_SAVE_VIEW_SUBMIT,
  RUNS_SAVED_VIEW_CUSTOM,
  runsSavedViewOption,
} from "../../wardyn/copy/runs-landing";
import { DEFAULT_RUNS_FILTERS, matchesSavedView, type RunsFilterState, type RunsGroupBy, type RunsStatusFilter, type RunsWhoseRuns } from "./runs-filters";
import type { RunsEndedWithin } from "../../../lib/api/runs";
import type { SavedRunsView } from "./runs-saved-views";
import { activeRunsFilters } from "./runs-active-filters";

const WORKSPACE_ALL = "all";
// Never a real saved view's index — it is only ever the sentinel item shown
// when the current URL matches none of them (mock's "View · Custom"). Must
// be non-empty: a Radix SelectItem's value may never be "".
const CUSTOM_VIEW_VALUE = "custom";

type ControlProps = {
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
};

type BarProps = ControlProps & {
  onSaveView: (name: string) => void;
  /** How many runs the list shows now: the sheet's "Show N runs". */
  runCount: number;
  /** The list was cut at its limit: the count reads N+, as the page header does. */
  runsTruncated?: boolean;
};

// Every control but the search box and the Save view button. The same elements
// serve the inline bar and the phone sheet; `sheet` only widens the triggers
// to the sheet's width and gives the selects the sheet's larger touch size.
function buildControls(p: ControlProps, sheet: boolean) {
  const { filters, workspaces, adminView, currentSearch, savedViews, onChange, onSelectView } = p;
  const width = (inline: string) => (sheet ? "w-full" : inline);
  const size = sheet ? "default" : "sm";
  // Indexed by position, not by its own search string — "Default" is itself
  // the empty query string, and a Radix SelectItem's value may never be "".
  // matchesSavedView ignores `owner` in the Admin view: applySavedViewOwner
  // (runs.tsx) reapplies the CURRENT Everyone/Mine onto a picked view's own
  // search, so the URL right after a pick carries `owner=me` under Mine even
  // though no built-in view ever stores that param.
  const selectedIndex = savedViews.findIndex((v) => matchesSavedView(v.search, currentSearch, adminView));
  const selectedView = selectedIndex >= 0 ? String(selectedIndex) : CUSTOM_VIEW_VALUE;

  const status = (
    <Select value={filters.status} onValueChange={(v) => onChange({ ...filters, status: v as RunsStatusFilter })}>
      <SelectTrigger size={size} className={width("w-[160px]")} aria-label={RUNS_FILTERS.STATUS_LABEL}>
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
  );

  const ended = (
    <Select value={filters.endedWithin} onValueChange={(v) => onChange({ ...filters, endedWithin: v as RunsEndedWithin })}>
      <SelectTrigger size={size} className={width("w-[160px]")} aria-label={RUNS_FILTERS.ENDED_WITHIN_LABEL}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="24h">{RUNS_FILTERS.ENDED_24H}</SelectItem>
        <SelectItem value="7d">{RUNS_FILTERS.ENDED_7D}</SelectItem>
        <SelectItem value="30d">{RUNS_FILTERS.ENDED_30D}</SelectItem>
        <SelectItem value="all">{RUNS_FILTERS.ENDED_ALL}</SelectItem>
      </SelectContent>
    </Select>
  );

  const workspace = (
    <Select value={filters.workspace} onValueChange={(v) => onChange({ ...filters, workspace: v })}>
      <SelectTrigger size={size} className={width("w-[190px]")} aria-label={RUNS_FILTERS.WORKSPACE_LABEL}>
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
  );

  const group = (
    <Select value={filters.group} onValueChange={(v) => onChange({ ...filters, group: v as RunsGroupBy })}>
      <SelectTrigger size={size} className={width("w-[160px]")} aria-label={RUNS_FILTERS.GROUP_LABEL}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="sections">{RUNS_FILTERS.GROUP_SECTIONS}</SelectItem>
        <SelectItem value="workspace">{RUNS_FILTERS.GROUP_WORKSPACE}</SelectItem>
        <SelectItem value="title">{RUNS_FILTERS.GROUP_TITLE}</SelectItem>
      </SelectContent>
    </Select>
  );

  // H-4, Admin view only: the User view always shows the caller's own
  // runs and has no control for it.
  const scope = adminView ? (
    <Select value={filters.scope} onValueChange={(v) => onChange({ ...filters, scope: v as RunsWhoseRuns })}>
      <SelectTrigger size={size} className={width("w-[140px]")} aria-label={RUNS_FILTERS.WHOSE_RUNS_LABEL}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="all">{RUNS_FILTERS.EVERYONE}</SelectItem>
        <SelectItem value="mine">{RUNS_FILTERS.MINE}</SelectItem>
      </SelectContent>
    </Select>
  ) : null;

  const killed = (
    <label className="flex items-center gap-1.5 text-meta text-muted-foreground">
      <input
        type="checkbox"
        checked={filters.includeKilled}
        onChange={(e) => onChange({ ...filters, includeKilled: e.target.checked })}
      />
      {RUNS_INCLUDE_KILLED}
    </label>
  );

  // H-8: saved views. Selecting one is a plain navigation to its own
  // query string (a saved view is a complete state, not a merge).
  const view = (
    <Select
      value={selectedView}
      onValueChange={(v) => {
        if (v === CUSTOM_VIEW_VALUE) return; // informational only, mock (:615) offers no action for it
        const picked = savedViews[Number(v)];
        if (picked) onSelectView(picked.search);
      }}
    >
      <SelectTrigger size={size} className={sheet ? "min-w-0 flex-1" : "w-[170px]"} aria-label={RUNS_FILTERS.SAVED_VIEW_LABEL}>
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
  );

  return { status, ended, workspace, group, scope, killed, view };
}

function SaveViewButton({ onClick, className }: { onClick: () => void; className?: string }) {
  return (
    <Button type="button" size="sm" variant="outline" className={className} onClick={onClick}>
      {RUNS_SAVE_VIEW}
    </Button>
  );
}

function SaveViewForm({ onSave, onClose }: { onSave: (name: string) => void; onClose: () => void }) {
  const [name, setName] = React.useState(RUNS_SAVE_VIEW_DEFAULT_NAME);
  const id = React.useId();
  return (
    <form
      className="flex w-full flex-wrap items-center gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        onSave(name.trim() || RUNS_SAVE_VIEW_DEFAULT_NAME);
        onClose();
      }}
    >
      <label htmlFor={id} className="shrink-0 whitespace-nowrap text-meta text-muted-foreground">
        {RUNS_SAVE_VIEW_NAME_LABEL}
      </label>
      <Input
        id={id}
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
      <Button type="button" size="sm" variant="ghost" onClick={onClose}>
        {RUNS_SAVE_VIEW_CANCEL}
      </Button>
    </form>
  );
}

// #3041: the active filters as removable chips under the search box.
function ActiveFilterChips({ filters, adminView, onChange }: Pick<ControlProps, "filters" | "adminView" | "onChange">) {
  const active = activeRunsFilters(filters, adminView);
  if (active.length === 0) return null;
  return (
    <div className="flex w-full flex-wrap gap-2 sm:hidden" data-testid="runs-filter-chips">
      {active.map((a) => (
        <Chip key={a.key} tone="neutral" className="h-7 gap-1 rounded-full pr-1 text-meta">
          {a.label}
          <button
            type="button"
            aria-label={RUNS_FILTERS_SHEET.CHIP_REMOVE_ARIA(a.label)}
            onClick={() => onChange(a.without)}
            className="grid size-5 place-items-center rounded-full text-muted-foreground hover:text-foreground"
          >
            <X className="size-3.5" aria-hidden />
          </button>
        </Chip>
      ))}
    </div>
  );
}

function showRuns(n: number, more?: boolean): string {
  if (more) return RUNS_FILTERS_SHEET.SHOW_MORE(n);
  return n === 1 ? RUNS_FILTERS_SHEET.SHOW_ONE : RUNS_FILTERS_SHEET.SHOW(n);
}

// #3041: the phone's Filters (n) button and the bottom sheet it opens. The
// sheet's controls apply as they change (the URL is the state); "Show N runs"
// just closes it.
function FiltersSheet({
  runCount,
  runsTruncated,
  onSaveView,
  searchRef,
  ...p
}: ControlProps & Pick<BarProps, "runCount" | "runsTruncated" | "onSaveView"> & { searchRef: React.RefObject<HTMLInputElement | null> }) {
  const [saving, setSaving] = React.useState(false);
  const [open, setOpen] = React.useState(false);
  // One way out for every close: a controlled `open` change does not fire
  // onOpenChange, so the resize path below must clear the Save view form too.
  const setSheetOpen = (o: boolean) => {
    setOpen(o);
    if (!o) setSaving(false);
  };
  // The sheet is phone-only. Its overlay and focus trap are not, so an open
  // sheet must close when the viewport grows to `sm` (a rotated phone, a
  // resized window) or it would leave the page dimmed and locked.
  React.useEffect(() => {
    const mq = window.matchMedia?.("(min-width: 640px)"); // jsdom has no matchMedia
    if (!mq) return;
    const onChange = () => mq.matches && setSheetOpen(false);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);
  const { filters, adminView, onChange } = p;
  const c = buildControls(p, true);
  const filterCount = activeRunsFilters(filters, adminView).length;
  const field = (label: string, control: React.ReactNode) => (
    <div className="flex min-w-0 flex-col gap-1.5">
      <span className="text-meta text-muted-foreground">{label}</span>
      {control}
    </div>
  );
  return (
    <Sheet open={open} onOpenChange={setSheetOpen}>
      <SheetTrigger asChild>
        <Button type="button" variant="outline" className="shrink-0 sm:hidden">
          {filterCount > 0 ? RUNS_FILTERS_SHEET.BUTTON_COUNT(filterCount) : RUNS_FILTERS_SHEET.TITLE}
        </Button>
      </SheetTrigger>
      <SheetContent
        side="bottom"
        aria-describedby={undefined}
        // Closed by growing past `sm`, the Filters button is display:none and
        // cannot take focus back: the search box, the one control left on
        // every width, does. A close at phone width keeps Radix's own return
        // to the button.
        onCloseAutoFocus={(e) => {
          if (!window.matchMedia?.("(min-width: 640px)").matches) return;
          e.preventDefault();
          searchRef.current?.focus();
        }}
        className="max-h-[90dvh] gap-3 rounded-t-2xl p-4 pt-3 sm:hidden"
      >
        <div className="mx-auto h-1 w-9 rounded-full bg-border-strong" aria-hidden />
        <SheetHeader className="p-0">
          <SheetTitle className="text-base">{RUNS_FILTERS_SHEET.TITLE}</SheetTitle>
        </SheetHeader>
        {field(RUNS_FILTERS.STATUS_LABEL, c.status)}
        <div className="grid grid-cols-2 gap-2.5">
          {field(RUNS_FILTERS.ENDED_WITHIN_LABEL, c.ended)}
          {field(RUNS_FILTERS.WORKSPACE_LABEL, c.workspace)}
          {field(RUNS_FILTERS.GROUP_LABEL, c.group)}
          {c.scope && field(RUNS_FILTERS.WHOSE_RUNS_LABEL, c.scope)}
        </div>
        {c.killed}
        {field(
          RUNS_FILTERS.SAVED_VIEW_LABEL,
          <div className="flex items-center gap-2">
            {c.view}
            <SaveViewButton onClick={() => setSaving(true)} />
          </div>,
        )}
        {saving && <SaveViewForm onSave={onSaveView} onClose={() => setSaving(false)} />}
        <div className="mt-1 flex items-center gap-2.5">
          <Button type="button" variant="ghost" size="lg" onClick={() => onChange({ ...DEFAULT_RUNS_FILTERS, q: filters.q })}>
            {RUNS_FILTERS_SHEET.CLEAR}
          </Button>
          <SheetClose asChild>
            <Button type="button" size="lg" className="flex-1">
              {showRuns(runCount, runsTruncated)}
            </Button>
          </SheetClose>
        </div>
      </SheetContent>
    </Sheet>
  );
}

export function RunsFilterBar({ runCount, runsTruncated, onSaveView, ...p }: BarProps) {
  const { filters, onChange } = p;
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
  const c = buildControls(p, false);
  const searchRef = React.useRef<HTMLInputElement>(null);

  return (
    <div className="mt-3.5 flex flex-wrap items-center gap-2" role="search">
      <div className="relative w-full max-w-xs max-sm:w-auto max-sm:min-w-0 max-sm:flex-1 max-sm:max-w-none">
        <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          ref={searchRef}
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

      <FiltersSheet {...p} runCount={runCount} runsTruncated={runsTruncated} onSaveView={onSaveView} searchRef={searchRef} />
      <ActiveFilterChips filters={filters} adminView={p.adminView} onChange={onChange} />

      {/* `contents` keeps these as direct flex items of the bar, as before the
          phone layout; under `sm` the sheet holds them instead. */}
      <div className="contents max-sm:hidden">
        {c.status}
        {c.ended}
        {c.workspace}
        {c.group}
        {c.scope}
        {c.killed}
        {c.view}
        <SaveViewButton onClick={() => setSaving(true)} />
      </div>

      {saving && (
        <div className="flex w-full max-sm:hidden">
          <SaveViewForm onSave={onSaveView} onClose={() => setSaving(false)} />
        </div>
      )}
    </div>
  );
}
