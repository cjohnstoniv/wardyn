/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// FOCUS MODE — design board 2c ("Focus"). The session owns the pixels and
// everything else is glass over it: the terminal goes full-bleed, the shell's
// header and sidebar are gone (app-shell's FocusContext, set by the canvas),
// what chrome remains floats as a HUD, and the widgets move into an edge dock
// that slides over the session rather than taking width from it.
//
// Two things this deliberately is NOT:
//
//  1. It is NOT the terminal's fullscreen. AttachTerminal uses the NATIVE
//     Fullscreen API (see its long comment) precisely because a `fixed inset-0`
//     overlay is at the mercy of any ancestor that establishes a containing
//     block. This overlay dodges that the other way — it PORTALS to
//     document.body, so it has no such ancestor — and the two compose: you can
//     take the terminal to real fullscreen from inside focus mode, and the
//     browser's own Escape handling unwinds that first (it never reaches this
//     module's handler).
//
//  2. It does NOT reproduce chrome it does not own. The board's HUD also
//     carries the four tabs and Kill; those live in run-detail-command-bar.tsx
//     and run-detail-summary-header.tsx, which this lane may not touch, so the
//     HUD states the run identity it CAN derive from the widget context and
//     leaves the rest to the normal cockpit. Same rule as the shortcut strip
//     below: no affordance that only pretends to work.
import * as React from "react";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { createPortal } from "react-dom";
import { Minimize2, PanelRightClose } from "lucide-react";
import { cn } from "../../ui/utils";
import { ConfinementChip } from "../../wardyn/primitives";
import { Kbd, MOD } from "../../wardyn/kbd";
import { RUN_WIDGETS, WIDGET_IDS, type WidgetContext, type WidgetId } from "./widget-registry";

// Glass: the board's rgba panel + backdrop blur, in theme tokens so it survives
// a light theme instead of being a hard-coded dark rgba.
const GLASS = "border border-border bg-popover/85 backdrop-blur-md";

/** The dock's own copy of the FILL_TILE trick from canvas.tsx: a WidgetCard is
 *  a <section>, so stretch it and let its body scroll. */
const FILL = [
  "[&>section]:min-h-0 [&>section]:flex-1",
  "[&>section>*:last-child]:min-h-0 [&>section>*:last-child]:flex-1",
  "[&>section>*:last-child]:overflow-y-auto",
].join(" ");

export function FocusMode({ ctx, onExit }: { ctx: WidgetContext; onExit: () => void }) {
  // "the dock carries the same widget set rather than a reduced one" (the
  // board's own note) — every widget except the hero, availability-gated the
  // same way the canvas gates its tiles.
  const dockable = WIDGET_IDS.filter(
    (id) => id !== "terminal" && (RUN_WIDGETS[id].available?.(ctx) ?? true),
  );
  const first = dockable[0] ?? null;
  // ONE piece of state, not an open flag plus a selection: null IS closed.
  const [dock, setDock] = React.useState<WidgetId | null>(first);

  React.useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      // WCAG 2.1.2 — no keyboard trap. Capture phase, so it runs before xterm's
      // textarea swallows the key and sends it to the PTY as literal input;
      // that costs a vim user their Escape while focus mode is on, which is the
      // same trade AttachTerminal's fullscreen already makes (and the strip
      // says so). In NATIVE fullscreen the browser exits first and this never
      // fires, so the two do not fight.
      if (e.key === "Escape" && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
        e.preventDefault();
        e.stopPropagation();
        onExit();
        return;
      }
      // ⌘\ / Ctrl+\ — the board's dock shortcut. ponytail: Ctrl+\ is SIGQUIT in
      // a PTY, so focus mode costs you that one key too; the dock is also a
      // plain button, so nothing depends on the shortcut.
      if ((e.metaKey || e.ctrlKey) && e.key === "\\") {
        e.preventDefault();
        e.stopPropagation();
        setDock((cur) => (cur ? null : first));
      }
    };
    document.addEventListener("keydown", onKeyDown, true);
    return () => document.removeEventListener("keydown", onKeyDown, true);
  }, [onExit, first]);

  return createPortal(
    // z-40: above the cockpit's own toolbars (z-30) and the shell, but BELOW
    // app-shell's unreachable-daemon banner (z-50), which must stay visible.
    <div className="fixed inset-0 z-40 flex flex-col bg-background text-foreground">
      <div className="relative min-h-0 flex-1">
        {/* Full-bleed: the padding only keeps the session clear of the HUD pill
            above it and the dock rail beside it. */}
        <div className="flex h-full min-h-0 flex-col py-3 pl-4 pr-[4.75rem] pt-16">
          {ctx.terminalPane}
        </div>

        <div className={cn("absolute left-4 top-3 flex items-center gap-3 rounded-xl px-3 py-1.5", GLASS)}>
          <button
            type="button"
            onClick={onExit}
            className="inline-flex h-7 items-center gap-1.5 rounded-lg border border-border px-2 text-xs font-medium text-foreground hover:bg-accent"
          >
            <Minimize2 className="size-3.5" aria-hidden />
            {RUN_COCKPIT.exitFocus}
          </button>
          <span className="h-5 w-px bg-border" />
          <span className="max-w-[24rem] truncate font-mono text-sm text-foreground">{ctx.run.repo}</span>
          <span className="font-mono text-xs text-muted-foreground">{ctx.run.id.slice(0, 8)}</span>
        </div>

        <Dock ctx={ctx} ids={dockable} open={dock} onOpen={setDock} />
      </div>

      <Strip ctx={ctx} />
    </div>,
    document.body,
  );
}

// The edge dock: a permanent icon rail (the affordance that opens it again once
// it is closed — otherwise ⌘\ would be the only way back) and a panel that
// slides OVER the session rather than taking width from it.
function Dock({
  ctx,
  ids,
  open,
  onOpen,
}: {
  ctx: WidgetContext;
  ids: WidgetId[];
  open: WidgetId | null;
  onOpen: (id: WidgetId | null) => void;
}) {
  const def = open ? RUN_WIDGETS[open] : null;
  return (
    <>
      {def && (
        <section
          aria-label={def.label}
          className={cn(
            "absolute bottom-3 right-[4.5rem] top-16 flex w-[min(26rem,55vw)] flex-col rounded-xl p-3 shadow-floating",
            GLASS,
          )}
        >
          <div className={cn("flex min-h-0 flex-1 flex-col", FILL)}>{def.component(ctx)}</div>
        </section>
      )}

      <div
        role="group"
        aria-label={RUN_COCKPIT.dock}
        className={cn(
          "absolute bottom-3 right-3 top-16 flex w-13 flex-col items-center gap-1.5 rounded-xl py-2",
          GLASS,
        )}
      >
        {ids.map((id) => {
          const w = RUN_WIDGETS[id];
          const on = open === id;
          return (
            <button
              key={id}
              type="button"
              aria-pressed={on}
              aria-label={RUN_COCKPIT.showWidget(w.label)}
              onClick={() => onOpen(on ? null : id)}
              className={cn(
                "flex size-9 items-center justify-center rounded-lg",
                on
                  ? "border border-primary/40 bg-primary/10 text-primary"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              <w.Icon className="size-4" aria-hidden />
            </button>
          );
        })}
        {open && (
          <button
            type="button"
            onClick={() => onOpen(null)}
            aria-label={RUN_COCKPIT.closeDock}
            className="mt-auto flex size-9 items-center justify-center rounded-lg text-muted-foreground hover:text-foreground"
          >
            <PanelRightClose className="size-4" aria-hidden />
          </button>
        )}
      </div>
    </>
  );
}

// The bottom strip. Every fact here is already on the canvas somewhere; the
// point of the board's strip is that in focus mode you can read them without
// opening anything. Derived from the SAME context the widgets read, so the two
// cannot disagree.
function Strip({ ctx }: { ctx: WidgetContext }) {
  const allow = ctx.egress.filter((e) => e.decision === "allow").length;
  const held = ctx.egress.filter((e) => e.decision === "pending").length;
  const deny = ctx.egress.filter((e) => e.decision === "deny").length;
  // The outcome filter is load-bearing, not decorative: the broker audits
  // DENIED mint attempts under this same action, and rendering one as
  // "brokered" would claim a credential that was never issued.
  const brokered = ctx.audit.filter(
    (e) => e.action === "credential.mint" && e.outcome === "success",
  ).length;

  return (
    <div className="flex h-9 shrink-0 items-center gap-4 border-t border-border bg-card/80 px-5 font-mono text-meta backdrop-blur">
      <span className="text-muted-foreground">
        {RUN_COCKPIT.egress} <span className="text-success">{RUN_COCKPIT.allow(allow)}</span> ·{" "}
        <span className="text-warning">{RUN_COCKPIT.held(held)}</span> ·{" "}
        <span className="text-danger">{RUN_COCKPIT.deny(deny)}</span>
      </span>
      <span className="text-muted-foreground">
        {RUN_COCKPIT.credentials} <span>{RUN_COCKPIT.eligible(ctx.grants.length)}</span> ·{" "}
        <span className="text-info">{RUN_COCKPIT.brokered(brokered)}</span>
      </span>
      {/* The Fence/Wall/Vault metal ramp, via the one chip that owns it — never
          the teal accent, which means "action" everywhere else in this
          console. */}
      <span className="flex items-center gap-2">
        <ConfinementChip value={ctx.run.confinement_class} />
        <span className="text-muted-foreground">{ctx.run.runner_target}</span>
      </span>
      {/* M3: two labelled key chips instead of one run of text — the chord is
          drawn by Kbd, which spells the modifier for the platform, so a Windows
          or Linux operator is no longer told to press a key their keyboard does
          not have. Still only the two chords this file actually binds. */}
      <span className="ml-auto flex shrink-0 items-center gap-3 text-muted-foreground">
        <span className="flex items-center gap-1.5">
          <Kbd keys={[MOD, "\\"]} />
          {RUN_COCKPIT.shortcutDock}
        </span>
        <span className="flex items-center gap-1.5">
          <Kbd keys={["Esc"]} />
          {RUN_COCKPIT.shortcutExitFocus}
        </span>
      </span>
    </div>
  );
}
