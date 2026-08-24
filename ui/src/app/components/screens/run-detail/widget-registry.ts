/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// WIDGET REGISTRY — the one place that says what a cockpit widget IS: its
// catalog label, its icon, how to render it, how big it comes out of the
// catalog, and where the two situational presets put it. The canvas, the
// catalog popover and the preset buttons all read this table, so adding a
// widget is one entry here rather than three edits in three files.
//
// The ids are NOT ours to choose: internal/api/ui_layout.go's
// runLayoutWidgetIDs is a CLOSED set the server validates every PUT against
// ("terminal, egress, files, sandbox, credentials, identity, ssh"). An id
// outside it is a 400, so this table's keys must match that slice exactly —
// the two halves of the contract are kept in sync by hand. That file's own
// doc explains what is deliberately absent: `timeline` (deleted by the
// terminal-first redesign) and the three TABS, which have no x/y/w/h.
//
// No JSX here on purpose — this is a .ts data table, so the render functions
// use React.createElement rather than turning the registry into a component
// module.
import * as React from "react";
import { Box, FileDiff, Fingerprint, Globe, KeyRound, SquareTerminal, Terminal } from "lucide-react";
import type {
  AgentRun,
  AuditEvent,
  CredentialGrant,
  EgressDecision,
} from "../../../lib/types";
import type { RunLayoutPreset, RunLayoutWidget } from "../../../lib/api/run-layout";
import { ConnectSSHCard } from "../run-detail-ssh";
import {
  CredentialsWidget,
  EgressWidget,
  FilesChangedWidget,
  IdentityWidget,
  SandboxWidget,
} from "./widgets";

// The canvas grid. 12 columns is the house convention; 12 ROWS is what one
// viewport is worth — canvas.tsx derives rowHeight from the measured pane
// height so a preset that adds up to 12 rows fills the cockpit exactly and
// nothing scrolls. Anything past row 12 is below the canvas fold (the
// evidence rail worked the same way before this: it scrolled).
export const GRID_COLS = 12;
export const GRID_ROWS = 12;

export type WidgetId =
  | "terminal"
  | "egress"
  | "files"
  | "sandbox"
  | "credentials"
  | "identity"
  | "ssh";

// Everything a widget can need, assembled once by the screen. Widgets keep
// their own props — this is only the bag the registry destructures from, so a
// widget's signature never has to know about the canvas.
export type WidgetContext = {
  run: AgentRun;
  /** The run has stopped (isTerminalRunState). Inverted for the polling
   *  widgets' `live` prop, which asks the opposite question. */
  finished: boolean;
  /** The signed-in principal, for the ssh widget's owner gate. */
  principal: string | null;
  grants: CredentialGrant[];
  egress: EgressDecision[];
  audit: AuditEvent[];
  onGoAudit: () => void;
  /** The terminal hero, built by run-detail.tsx — it owns AttachTerminal, the
   *  replay fallback and the held-approval strip that sits under the output.
   *  The registry only PLACES it; teaching this table to build it would drag
   *  the whole recording/attach fetch graph into a data file. */
  terminalPane: React.ReactNode;
};

export type WidgetPlacement = { x: number; y: number; w: number; h: number };

export type WidgetDef = {
  /** Catalog label. Deliberately the widget's own card title, so the catalog
   *  and the tile never call the same thing two different names. */
  label: string;
  Icon: React.ElementType;
  component: (ctx: WidgetContext) => React.ReactNode;
  /** Size (and floor) used when the catalog ADDS this widget. */
  defaultLayout: { w: number; h: number; minW: number; minH: number };
  /** Where each preset puts it. A preset that omits the widget does not place
   *  it at all — a stopped run has no sandbox to meter and nothing to attach
   *  to, so `finished` leaves both out (the catalog can still add them). */
  presets: Partial<Record<RunLayoutPreset, WidgetPlacement>>;
  /** False = render no tile at all right now. Only ssh needs this: its card
   *  returns null unless you own a RUNNING run, and a null inside a grid tile
   *  is an empty box with a dot grid behind it, not nothing. Mirrors
   *  ConnectSSHCard's own gate. */
  available?: (ctx: WidgetContext) => boolean;
  /** Cannot be removed — see removeWidget below. */
  required?: boolean;
};

// THE TERMINAL IS THE HERO and the layout may not take that away: `required`
// makes the catalog refuse to remove it and normalizeLayout put it back if a
// layout arrives without it, and minW/minH keep a resize from shrinking it to
// a stub. Both are cheap; the pair means neither a drag, a resize, a catalog
// click nor a hand-written saved row can end with a cockpit that has no
// session in it.
export const RUN_WIDGETS: Record<WidgetId, WidgetDef> = {
  terminal: {
    label: "Terminal",
    Icon: SquareTerminal,
    component: (ctx) => ctx.terminalPane,
    defaultLayout: { w: 8, h: 12, minW: 4, minH: 4 },
    presets: {
      // Live: the phase-1 cockpit exactly — session down the left, evidence
      // rail down the right.
      live: { x: 0, y: 0, w: 8, h: 12 },
      // Finished: the pane is a replay, so it yields half the width to the
      // diff the run actually produced.
      finished: { x: 0, y: 0, w: 7, h: 8 },
    },
    required: true,
  },
  egress: {
    label: "Egress",
    Icon: Globe,
    component: (ctx) => React.createElement(EgressWidget, { egress: ctx.egress, onGoAudit: ctx.onGoAudit }),
    defaultLayout: { w: 4, h: 4, minW: 3, minH: 2 },
    presets: {
      live: { x: 8, y: 0, w: 4, h: 4 },
      finished: { x: 7, y: 4, w: 5, h: 4 },
    },
  },
  files: {
    label: "Files changed",
    Icon: FileDiff,
    component: (ctx) => React.createElement(FilesChangedWidget, { runId: ctx.run.id, live: !ctx.finished }),
    defaultLayout: { w: 4, h: 4, minW: 3, minH: 2 },
    presets: {
      live: { x: 8, y: 4, w: 4, h: 4 },
      finished: { x: 7, y: 0, w: 5, h: 4 },
    },
  },
  sandbox: {
    label: "Sandbox",
    Icon: Box,
    component: (ctx) => React.createElement(SandboxWidget, { runId: ctx.run.id, live: !ctx.finished }),
    defaultLayout: { w: 4, h: 4, minW: 3, minH: 2 },
    presets: { live: { x: 8, y: 8, w: 4, h: 4 } },
  },
  credentials: {
    label: "Credentials",
    Icon: KeyRound,
    component: (ctx) => React.createElement(CredentialsWidget, { grants: ctx.grants, audit: ctx.audit }),
    defaultLayout: { w: 4, h: 4, minW: 3, minH: 2 },
    presets: {
      live: { x: 8, y: 12, w: 4, h: 4 },
      finished: { x: 0, y: 8, w: 6, h: 4 },
    },
  },
  identity: {
    label: "Identity",
    Icon: Fingerprint,
    component: (ctx) => React.createElement(IdentityWidget, { run: ctx.run }),
    defaultLayout: { w: 4, h: 4, minW: 3, minH: 2 },
    presets: {
      live: { x: 8, y: 16, w: 4, h: 4 },
      finished: { x: 6, y: 8, w: 6, h: 4 },
    },
  },
  ssh: {
    label: "Attach from your terminal",
    Icon: Terminal,
    component: (ctx) => React.createElement(ConnectSSHCard, { run: ctx.run }),
    defaultLayout: { w: 4, h: 5, minW: 3, minH: 4 },
    // LAST in the live rail on purpose: it is the one widget that can be
    // absent (owner-only), and with free placement an absent tile leaves a
    // hole — at the bottom of the rail that hole costs nothing, between two
    // widgets it is a visible gap on every run you did not start.
    presets: { live: { x: 8, y: 20, w: 4, h: 5 } },
    available: (ctx) =>
      ctx.run.state === "RUNNING" && !!ctx.principal && ctx.run.created_by === ctx.principal,
  },
};

export const WIDGET_IDS = Object.keys(RUN_WIDGETS) as WidgetId[];

function isWidgetId(id: string): id is WidgetId {
  return id in RUN_WIDGETS;
}

/** The situational default for a preset, in wire shape. */
export function presetLayout(preset: RunLayoutPreset): RunLayoutWidget[] {
  return WIDGET_IDS.flatMap((id) => {
    const at = RUN_WIDGETS[id].presets[preset];
    return at ? [{ widget: id, ...at }] : [];
  });
}

// The ONE guard every layout passes through — load, add, remove, preset
// switch. Drops ids this build cannot render (the server's set may outgrow
// the console's), drops duplicates (two tiles with the same key is a React
// bug, not a layout), and restores the terminal if a layout arrives without
// it. Fixing it here rather than at each call site is why "the terminal
// cannot be removed to nothing" needs no other enforcement.
export function normalizeLayout(
  layout: RunLayoutWidget[],
  preset: RunLayoutPreset,
): RunLayoutWidget[] {
  const seen = new Set<string>();
  const out: RunLayoutWidget[] = [];
  for (const w of layout) {
    if (!isWidgetId(w.widget) || seen.has(w.widget)) continue;
    seen.add(w.widget);
    out.push({ widget: w.widget, x: w.x, y: w.y, w: w.w, h: w.h });
  }
  for (const id of WIDGET_IDS) {
    if (RUN_WIDGETS[id].required && !seen.has(id)) {
      const at = RUN_WIDGETS[id].presets[preset] ?? {
        x: 0,
        y: 0,
        ...RUN_WIDGETS[id].defaultLayout,
      };
      out.unshift({ widget: id, x: at.x, y: at.y, w: at.w, h: at.h });
    }
  }
  return out;
}

/** Add a widget below everything already placed (full width of its default). */
export function addWidget(layout: RunLayoutWidget[], id: WidgetId): RunLayoutWidget[] {
  if (layout.some((w) => w.widget === id)) return layout;
  const bottom = layout.reduce((y, w) => Math.max(y, w.y + w.h), 0);
  const { w, h } = RUN_WIDGETS[id].defaultLayout;
  return [...layout, { widget: id, x: 0, y: bottom, w, h }];
}

/** Remove a widget. A `required` widget is refused — same layout back. */
export function removeWidget(layout: RunLayoutWidget[], id: WidgetId): RunLayoutWidget[] {
  if (RUN_WIDGETS[id].required) return layout;
  return layout.filter((w) => w.widget !== id);
}
