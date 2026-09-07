/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ONE way to mount a widget from RUN_WIDGETS.
//
// The canvas and focus mode's dock are two renderers of one widget table, and
// they drifted: 6118c5c3 gave every CANVAS tile its own ErrorBoundary ("one
// broken widget can no longer blank the whole canvas") and the dock — which
// renders the same def.component(ctx) — was left bare, so a widget that threw
// there escaped to app-shell's boundary and replaced the entire main region,
// full-screen session included. The FILL trick had already been copy-pasted
// between the two for the same reason. Both now go through this slot, so the
// next fix to one is a fix to both by construction.
import * as React from "react";
import { cn } from "../../ui/utils";
import { ErrorBoundary } from "../../wardyn/error-boundary";

// Make any widget fill its slot without touching the widget files (none of them
// forwards a className): a WidgetCard renders a <section>, so stretch that, and
// give its BODY — the section's last child — the scroll it needs when the slot
// is smaller than its contents. Clipping evidence silently is the one thing the
// rail this replaces was explicitly built not to do.
//
// :only-child, NOT a bare `section` (R4-F142). Every entry in RUN_WIDGETS that
// wants this renders ONE root card — a WidgetCard, or the ssh tile's
// SectionCard — so "the widget's root" and "the slot's only element child" are
// the same node. The terminal widget is the exception: it returns a FRAGMENT
// (failure block, pane, approvals strip), and the bare selector caught the
// M7(b) failure block — a `shrink-0` <section> written to size to its content
// above the terminal — and gave it `flex: 1 1 0%`. Measured in Chromium: 271px
// of a 518px tile, half the replay pane, on every KILLED/FAILED run, and a
// 271px bordered card holding nothing but an "Open audit trail" button when the
// ending is one this build does not recognise. A widget that renders siblings
// now sizes them itself, which is the only place that knows how.
export const FILL_TILE = [
  "[&>section:only-child]:min-h-0 [&>section:only-child]:flex-1",
  "[&>section:only-child>*:last-child]:min-h-0 [&>section:only-child>*:last-child]:flex-1",
  "[&>section:only-child>*:last-child]:overflow-y-auto",
].join(" ");

/** A widget in its box: the FILL stretch plus its own error boundary, keyed on
 *  the run so a stale crash from a PREVIOUS run cannot survive switching. */
export function WidgetSlot({
  region,
  resetKey,
  className,
  children,
}: {
  region: string;
  resetKey: string | number;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("flex min-h-0 flex-1 flex-col", FILL_TILE, className)}>
      <ErrorBoundary region={region} resetKey={resetKey}>
        {children}
      </ErrorBoundary>
    </div>
  );
}
