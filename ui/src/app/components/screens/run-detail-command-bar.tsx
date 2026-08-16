/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RunDetailCommandBar — the 36px tabs row directly under SummaryHeader's
// 52px command bar (design board seg2a's `h-9` row). This component owns
// only the row's layout: the four TabsTriggers on the left are Radix Tabs
// content, and Tabs.List/Tabs.Trigger must live inside the parent's own
// <Tabs> root (Radix context) — so the parent builds that markup (including
// the pending-approvals count badge on the Approvals trigger) and hands it
// in as `tabs`, rather than this component importing the Tabs primitives
// itself.
import * as React from "react";
import { RUN_COCKPIT } from "../wardyn/copy";
import { LayoutGrid, Pencil, Plus } from "lucide-react";
import { cn } from "../ui/utils";

export function RunDetailCommandBar({
  tabs,
  className,
}: {
  /** The parent's <TabsList>…</TabsList>, rendered inside its own <Tabs> root. */
  tabs: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex h-9 shrink-0 items-center gap-2 border-b border-border bg-background px-4",
        className,
      )}
    >
      <div className="flex min-w-0 items-center gap-0.5">{tabs}</div>

      {/* Layout controls from the design board — PHASE-2, no layout engine
          exists yet (WidgetCard's own doc calls out the "layout engine in
          phase 2" drag handle). Rendered disabled/inert, matching the
          board's markup verbatim, so the row reads as "coming soon" rather
          than a broken control. Do not wire these up here. */}
      <div className="ml-auto flex shrink-0 items-center gap-1.5" aria-hidden="true">
        <span className="inline-flex h-7 items-center gap-1.5 rounded-md border border-border px-2 font-mono text-[0.6875rem] text-muted-foreground opacity-60">
          <LayoutGrid className="size-3" />
          {RUN_COCKPIT.layoutPreset("Live")}
        </span>
        <button
          type="button"
          disabled
          tabIndex={-1}
          className="inline-flex h-7 items-center gap-1.5 rounded-md px-2 text-xs text-muted-foreground opacity-60 disabled:pointer-events-none"
        >
          <Plus className="size-3.5" />
          {RUN_COCKPIT.addWidget}
        </button>
        <button
          type="button"
          disabled
          tabIndex={-1}
          className="inline-flex h-7 items-center gap-1.5 rounded-md px-2 text-xs text-muted-foreground opacity-60 disabled:pointer-events-none"
        >
          <Pencil className="size-3.5" />
          {RUN_COCKPIT.editLayout}
        </button>
      </div>
    </div>
  );
}
