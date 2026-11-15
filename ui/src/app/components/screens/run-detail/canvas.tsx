/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run canvas — design board 2b. A free 12-column grid the operator arranges:
// tiles anywhere, drag from a
// header handle, resize from the corner, a snap ghost, a catalog, and two
// situational presets (Live / Finished). The arrangement is per USER and
// server-side, so it survives a new laptop.
//
// Layout invariants inherited from phase 1 and NOT negotiable here:
//   - the page does not scroll: this region is `min-h-0 flex-1` and owns its
//     own overflow, so app-shell's <main> never grows;
//   - the terminal is the hero and is ABOVE THE FOLD: it carries
//     data-testid="run-terminal-pane" and both presets place it at y=0 (e2e
//     asserts that geometrically, not just that a terminal rendered);
//   - the terminal cannot be arranged away — see widget-registry's `required`;
//   - every other widget lives in ONE host element per run, as the terminal does, and Expand moves that element from
//     its tile into a dialog (#3940). Same instance, so its state, polling and focus-free work survive the move.
//
// Grid engine: react-grid-layout v2 (`noCompactor`, so a tile stays where it
// was dropped instead of being sucked upward). Nothing already installed does
// free x/y placement with collision, a snap ghost and resize handles, and that
// is several hundred lines of pointer math to hand-roll badly.
import * as React from "react";
import { createPortal } from "react-dom";
import GridLayout, { noCompactor, type Layout } from "react-grid-layout";
import "react-grid-layout/css/styles.css";
import { Check, Expand, GripVertical, LayoutGrid, Pencil, Plus, RotateCcw, X } from "lucide-react";
import type { RunLayoutPreset } from "../../../lib/api/run-layout";
import { isVMKind } from "../../../lib/types/machine-kind";
import { Dialog, DialogContent, DialogTitle } from "../../ui/dialog";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import { cn } from "../../ui/utils";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { useFocusMode } from "../app-shell";
import { FocusMode } from "./focus-mode";
import { useRunLayout, type SaveStatus } from "./use-run-layout";
import { fitRows, measureTile, settleItems } from "./fit-layout";
import { WidgetSlot } from "./widget-slot";
import { ErrorBoundary } from "../../wardyn/error-boundary";
import { WidgetExpandContext } from "../../wardyn/primitives";
import {
  GRID_COLS,
  GRID_ROWS,
  RUN_WIDGETS,
  WIDGET_IDS,
  addWidget,
  presetLayout,
  removeWidget,
  type WidgetContext,
  type WidgetId,
} from "./widget-registry";

const MARGIN: [number, number] = [10, 10];
const PADDING: [number, number] = [10, 10];

// Before the first measurement the pane height is 0: size the rows as if it were this tall, so the first frame (and
// jsdom) has sane arithmetic. A measured pane is used as it is, whatever its height, because the grid is exactly
// 12 rows + gaps + padding and a floor above the pane makes the cockpit scroll (#3372).
const UNMEASURED_PANE = 520;

/** One twelfth of the pane, rounded DOWN so twelve rows plus their gaps never outgrow it (28px at least). */
export function canvasRowHeight(paneHeight: number): number {
  const pane = paneHeight === 0 ? UNMEASURED_PANE : paneHeight;
  return Math.max(28, Math.floor((pane - 2 * PADDING[1] - (GRID_ROWS - 1) * MARGIN[1]) / GRID_ROWS));
}

// react-grid-layout's stylesheet glides every tile for 200ms on any size or
// position change. That glide is for arranging; outside editing it slid a
// tile's controls out from under a press that had already landed (the release
// then hits something else and the click never fires). Inline, because that
// stylesheet is unlayered and outranks any utility class.
const STILL: React.CSSProperties = { transition: "none" };

// The board's dot grid, on only while editing — a permanent one would compete
// with the terminal.
const DOT_GRID: React.CSSProperties = {
  backgroundImage: "radial-gradient(var(--border) 1px, transparent 1px)",
  backgroundSize: "24px 24px",
  backgroundPosition: "12px 12px",
};

// The snap ghost. react-grid-layout's own stylesheet paints the placeholder
// solid red at 20% opacity; this overrides it with the board's dashed teal.
// The doubled class in the selector (`.react-grid-item.react-grid-placeholder`)
// out-specifies that rule no matter which stylesheet the bundler emits first.
const GHOST = [
  "[&_.react-grid-item.react-grid-placeholder]:rounded-lg",
  "[&_.react-grid-item.react-grid-placeholder]:border-2",
  "[&_.react-grid-item.react-grid-placeholder]:border-dashed",
  "[&_.react-grid-item.react-grid-placeholder]:border-primary/60",
  "[&_.react-grid-item.react-grid-placeholder]:bg-primary/10",
  "[&_.react-grid-item.react-grid-placeholder]:opacity-100",
].join(" ");

/** Measures the scroll box: width for the grid, height to size a row.
 *  A callback ref rather than useRef, because focus mode unmounts this box and
 *  brings it back: a `[]` effect would keep observing the DETACHED node and
 *  every row height after the first exit would be stale. A layout effect, so
 *  the first frame painted is already at the measured size: a passive effect
 *  painted the tiles at the 480px floor first and resized them a frame later,
 *  moving every control in them after it appeared. */
function useCanvasSize() {
  const [node, setNode] = React.useState<HTMLDivElement | null>(null);
  const [size, setSize] = React.useState({ width: 0, height: 0 });
  React.useLayoutEffect(() => {
    if (!node) return;
    const measure = () =>
      setSize((prev) =>
        prev.width === node.clientWidth && prev.height === node.clientHeight
          ? prev
          : { width: node.clientWidth, height: node.clientHeight },
      );
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(node);
    return () => ro.disconnect();
  }, [node]);
  return { ref: setNode, node, width: size.width, height: size.height };
}

/** Where the terminal shows right now: the cockpit tile or the focus overlay.
 *  It renders only an empty box and moves the run's one host element into it,
 *  so the terminal itself is never remounted by moving between the two. */
function TerminalSlot({ host, className = "flex min-h-0 flex-1 flex-col" }: { host: HTMLElement; className?: string }) {
  const attach = React.useCallback(
    (box: HTMLDivElement | null) => {
      if (box) box.appendChild(host);
    },
    [host],
  );
  return <div ref={attach} className={className} />;
}

// Scroll offsets of a widget's host, read when its box lets go of it and written back when the next box takes it.
const HOST_SCROLL = new WeakMap<HTMLElement, [Element, number, number][]>();

/** Where a widget shows right now: its tile or the large view. Like TerminalSlot it renders only an empty box and moves
 *  the widget's one host element into it, so the widget is never remounted. Moving an element resets the scroll offset
 *  of everything inside it, and by the time the next box attaches, React has already removed the old one and the host is
 *  disconnected (every offset reads 0). So the offsets are read when the old box detaches, while the host is still in
 *  the page, and written back on attach (the browser clamps them to the new size). */
function WidgetBox({ host }: { host: HTMLElement }) {
  const attach = React.useCallback(
    (box: HTMLDivElement | null) => {
      if (!box) {
        const kept: [Element, number, number][] = [];
        host.querySelectorAll("*").forEach((el) => {
          if (el.scrollTop || el.scrollLeft) kept.push([el, el.scrollTop, el.scrollLeft]);
        });
        HOST_SCROLL.set(host, kept);
        return;
      }
      box.appendChild(host);
      for (const [el, top, left] of HOST_SCROLL.get(host) ?? []) {
        el.scrollTop = top;
        el.scrollLeft = left;
      }
      HOST_SCROLL.delete(host);
    },
    [host],
  );
  return <div ref={attach} className="flex min-h-0 flex-1 flex-col" />;
}

// The large view's own box: 8px below the shell's top bar and banners (--shell-banner-h, 56px with no banner) and 44px
// from the bottom, so a daemon banner stays whole above it. It grows with its widget up to that height and scrolls inside it. No exit animation:
// the widget is back in its tile in the commit that closes the view, and a fade would show an empty box.
// --expand-min is the tallest it has been (set below); min-h can't outgrow the window, so a smaller window wins.
// bottom-auto: with DialogContent's inset-y-0 the box would no longer start at this top.
const EXPAND_DIALOG =
  "top-[calc(var(--shell-banner-h,3.5rem)_+_0.5rem)] flex h-auto max-h-[calc(100dvh_-_var(--shell-banner-h,3.5rem)_-_3.25rem)] min-h-[min(var(--expand-min,0px),calc(100dvh_-_var(--shell-banner-h,3.5rem)_-_3.25rem))] bottom-auto flex-col gap-0 overflow-hidden p-0 data-[state=closed]:animate-none sm:max-w-5xl";

const sameLayout = (a: { widget: string; x: number; y: number; w: number; h: number }[], b: typeof a) =>
  a.length === b.length && a.every((w, n) => w.widget === b[n].widget && w.x === b[n].x && w.y === b[n].y && w.w === b[n].w && w.h === b[n].h);

const sameRows = (a: Record<string, number>, b: Record<string, number>) => {
  const keys = Object.keys(b);
  return keys.length === Object.keys(a).length && keys.every((k) => a[k] === b[k]);
};

export function RunCanvas({ ctx }: { ctx: WidgetContext }) {
  // The situation picks the preset; the toolbar can override it while editing.
  // Interactive vs autonomous is NOT a third preset — that changes the Terminal
  // widget's own state, not the arrangement.
  const situational: RunLayoutPreset = ctx.finished ? "finished" : "live";
  const [preset, setPreset] = React.useState<RunLayoutPreset>(situational);
  const [editing, setEditing] = React.useState(false);
  // While editing, a situational flip (the run finishes mid-edit) must
  // not silently replace the in-progress arrangement on screen — the pending
  // PUT under editing already commits the arrangement to the OLD preset, so
  // the server never loses the edit. Re-syncs the moment editing ends.
  React.useEffect(() => {
    if (!editing) setPreset(situational);
  }, [situational, editing]);

  const [catalogOpen, setCatalogOpen] = React.useState(false);
  // The widget the person asked to see large; what is actually shown is `expandedId` below.
  const [expanded, setExpanded] = React.useState<WidgetId | null>(null);
  const [expandDialog, setExpandDialog] = React.useState<HTMLElement | null>(null);
  // The tile under the cursor, for the size readout. Only updated when its
  // dimensions actually change, so a drag is not a re-render per frame.
  const [active, setActive] = React.useState<{ i: string; w: number; h: number } | null>(null);

  const vm = isVMKind(ctx.run.machine_kind);
  const { layout, persistable, status, failedPreset, retry, apply, reset } = useRunLayout(preset, !!ctx.run.service, vm, !!ctx.run.machine);
  const { ref, node, width, height } = useCanvasSize();

  // Focus mode (design board 2c). The shell learns about it through
  // app-shell's FocusContext, which is also why this is safe outside the
  // console: a canvas mounted with no <AppShell> above it gets the default
  // no-op setter and simply never enters focus.
  const { focus, setFocus } = useFocusMode();
  // An ordinary exit (Exit focus, Escape, the leave chord) hands focus back to the button that opened it. That button
  // is a new element by then, so the effect below finds it after the cockpit has rendered. Unmounting skips this.
  const focusTriggerRef = React.useRef<HTMLButtonElement>(null);
  const restoreFocusRef = React.useRef(false);
  const exitFocus = React.useCallback(() => {
    restoreFocusRef.current = true;
    setFocus(false);
  }, [setFocus]);
  React.useEffect(() => {
    if (focus || !restoreFocusRef.current) return;
    restoreFocusRef.current = false;
    (focusTriggerRef.current ?? document.getElementById("main-content") ?? document.body).focus();
  }, [focus]);
  // Never leave the console headless: whatever unmounts this canvas — a route
  // change, an ErrorBoundary catch — puts the shell's chrome back.
  React.useEffect(() => () => setFocus(false), [setFocus]);

  // The terminal lives in ONE host element per run, outside the React tree the
  // layout switches between. The pane is portaled into it once (a fixed
  // container, so it never remounts) and TerminalSlot moves the element
  // between the cockpit tile and the focus overlay. Leaving the run screen
  // unmounts this canvas, which tears the terminal and its socket down.
  const runId = ctx.run.id;
  const host = React.useMemo(() => {
    const el = document.createElement("div");
    el.className = "flex min-h-0 flex-1 flex-col";
    el.dataset.runId = runId;
    return el;
  }, [runId]);
  React.useEffect(() => () => host.remove(), [host]);
  // The decision row gets the same persistent host, so entering or leaving focus mode moves its element instead of
  // remounting LiveApprovals (which would reset "Show all N" and the in-flight Approve/Deny state).
  const decisionHost = React.useMemo(() => {
    const el = document.createElement("div");
    el.className = "shrink-0";
    el.dataset.runId = runId;
    return el;
  }, [runId]);
  React.useEffect(() => () => decisionHost.remove(), [decisionHost]);
  const terminalPortal = createPortal(
    <ErrorBoundary region={RUN_WIDGETS.terminal.label} resetKey={runId}>
      {ctx.terminalPane}
    </ErrorBoundary>,
    host,
  );
  const decisionPortal = createPortal(
    <ErrorBoundary region={RUN_WIDGETS.terminal.label} resetKey={runId}>
      {ctx.decisionPane}
    </ErrorBoundary>,
    decisionHost,
  );
  // Every other widget gets a host of its own, made when first needed. The run page is keyed by run, so a canvas never
  // changes run while mounted.
  const widgetHosts = React.useMemo(() => {
    void runId; // a new run (never while mounted, in practice) starts with no hosts
    return new Map<WidgetId, HTMLElement>();
  }, [runId]);
  React.useEffect(() => () => widgetHosts.forEach((el) => el.remove()), [widgetHosts]);
  const hostOf = (id: WidgetId) => {
    let el = widgetHosts.get(id);
    if (!el) {
      el = document.createElement("div");
      el.className = "flex min-h-0 flex-1 flex-col";
      el.dataset.widgetHost = id;
      widgetHosts.set(id, el);
    }
    return el;
  };

  // A row is one twelfth of the visible pane, so a preset that adds up to
  // GRID_ROWS fills the cockpit exactly and nothing scrolls. Floors keep the
  // arithmetic sane before the first measurement (and under jsdom).
  // Floored at 480: below that the right column scrolls sideways inside the pane (#2909). Stacking the tiles is 0.10.
  const gridWidth = Math.max(width, 480);
  const rowHeight = canvasRowHeight(height);
  const colWidth = (gridWidth - 2 * PADDING[0] - (GRID_COLS - 1) * MARGIN[0]) / GRID_COLS;
  const sizeLabel = (w: number, h: number) =>
    `${Math.round(w * colWidth + (w - 1) * MARGIN[0])} × ${Math.round(h * rowHeight + (h - 1) * MARGIN[1])}`;

  // ONE renderability gate, and every consumer reads it. A widget can be placed
  // but not renderable right now (ssh on a run you do not own, or any finished
  // run). Keep it OUT of the grid — an empty tile over a dot grid is worse than
  // nothing — but keep its saved placement, or merely viewing someone else's
  // run would quietly delete it.
  const renderable = (id: WidgetId) => RUN_WIDGETS[id].available?.(ctx) ?? true;
  const visible = layout.filter((w) => renderable(w.widget as WidgetId));
  const hidden = layout.filter((w) => !visible.includes(w));
  // The widget shown in the large view. Derived, never stored or persisted: it can only be a widget that has a tile
  // right now (the same `visible` gate as the grid, which is what keeps a door the registry hides, such as the admin
  // monitor's SSH, shut when expanded), and never in focus mode or while arranging.
  const expandedId =
    !focus && !editing && expanded && expanded !== "terminal" && visible.some((w) => w.widget === expanded) ? expanded : null;
  // A widget that stops being expandable must not reopen on its own when it comes back.
  React.useEffect(() => {
    if (expanded && !expandedId) setExpanded(null);
  }, [expanded, expandedId]);
  const slotCtx = React.useMemo(
    () => ({
      ...ctx,
      terminalPane: <TerminalSlot host={host} />,
      // The dialog seats the decision row itself while a widget is expanded, so the held approvals are never behind it.
      decisionPane: expandedId ? null : <TerminalSlot host={decisionHost} className="shrink-0" />,
    }),
    [ctx, host, decisionHost, expandedId],
  );
  // One portal per tile that is not the terminal, only in this (non-focus) return, so focus mode unmounts them as it
  // does the grid and the dock keeps its own instance.
  const widgetPortals = visible
    .filter((w) => w.widget !== "terminal")
    .map((w) => {
      const id = w.widget as WidgetId;
      const def = RUN_WIDGETS[id];
      return createPortal(
        <WidgetExpandContext.Provider
          value={{ mode: id === expandedId ? "expanded" : editing ? "editing" : "tile", onExpand: () => setExpanded(id) }}
        >
          {/* WidgetSlot, not a bare div: the FILL stretch and the per-widget boundary are shared with focus mode's
              dock, which renders this same def.component(ctx). */}
          <WidgetSlot region={def.label} resetKey={ctx.run.id}>
            {def.component(slotCtx)}
          </WidgetSlot>
        </WidgetExpandContext.Provider>,
        hostOf(id),
        id,
      );
    });

  const items: Layout = visible.map((w) => ({
    i: w.widget,
    x: w.x,
    y: w.y,
    w: w.w,
    h: w.h,
    minW: RUN_WIDGETS[w.widget as WidgetId].defaultLayout.minW,
    minH: RUN_WIDGETS[w.widget as WidgetId].defaultLayout.minH,
  }));

  // #3038: a finished run nobody has rearranged reads at its content's size: each tile is as tall as what it holds, so
  // Identity is never cut off and an emptier tile is shorter. The terminal keeps its rows, and an arrangement the
  // person made, or one being edited, is shown exactly as stored.
  const fitOn = ctx.finished && !editing && sameLayout(layout, presetLayout("finished", false, vm, !!ctx.run.machine));
  // The default rail, unarranged, closes the rows of a tile this viewer can't see (Desktop and Attach on a run they did
  // not start). An arrangement the person made, or one being edited, is shown exactly as stored.
  const settleOn = !fitOn && !editing && sameLayout(layout, presetLayout(preset, !!ctx.run.service, vm, !!ctx.run.machine));
  const [fitted, setFitted] = React.useState<Record<string, number>>({});
  const visibleKey = visible.map((w) => w.widget).join();
  React.useLayoutEffect(() => {
    // While a widget is in the large view its tile is empty and would measure 0: leave `fitted` as it was.
    if (!fitOn || !node || expandedId) return;
    const measure = () => {
      const next: Record<string, number> = {};
      node.querySelectorAll<HTMLElement>("[data-widget]").forEach((tile) => {
        const px = measureTile(tile);
        if (px > 0) next[tile.dataset.widget!] = fitRows(px, rowHeight, MARGIN[1]);
      });
      setFitted((prev) => (sameRows(prev, next) ? prev : next));
    };
    measure();
    // A widget's own fetch lands inside its tile without the canvas rendering, so watch the tiles' contents too.
    const mo = new MutationObserver(measure);
    mo.observe(node, { childList: true, subtree: true, characterData: true });
    return () => mo.disconnect();
  }, [fitOn, node, rowHeight, gridWidth, visibleKey, expandedId]);
  const laid = fitOn ? settleItems(items, fitted, ["terminal"]) : settleOn ? settleItems(items, {}, []) : items;

  const commit = (next: Layout) => {
    apply([...next.map((it) => ({ widget: it.i, x: it.x, y: it.y, w: it.w, h: it.h })), ...hidden]);
    setActive(null);
  };

  const track = (_l: Layout, _o: unknown, item: { i: string; w: number; h: number } | null) => {
    if (!item) return;
    setActive((prev) =>
      prev && prev.i === item.i && prev.w === item.w && prev.h === item.h
        ? prev
        : { i: item.i, w: item.w, h: item.h },
    );
  };

  const toggleWidget = (id: WidgetId) => {
    apply(layout.some((w) => w.widget === id) ? removeWidget(layout, id) : addWidget(layout, id));
  };

  // The large view never shrinks while it is open, so a poll that shortens the widget cannot move Close or the content
  // under the pointer. It remembers the tallest it has been, and still gives way to a window smaller than that.
  React.useEffect(() => {
    if (!expandDialog || typeof ResizeObserver === "undefined") return;
    let tallest = 0;
    const ro = new ResizeObserver(() => {
      const h = expandDialog.offsetHeight;
      if (h <= tallest) return;
      tallest = h;
      expandDialog.style.setProperty("--expand-min", `${h}px`);
    });
    ro.observe(expandDialog);
    return () => ro.disconnect();
  }, [expandDialog]);

  if (focus) {
    return (
      <>
        {terminalPortal}
        {decisionPortal}
        <FocusMode ctx={slotCtx} onExit={exitFocus} />
      </>
    );
  }

  return (
    <>
      {terminalPortal}
      {decisionPortal}
      {widgetPortals}
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
        <div
          ref={ref}
          data-testid="run-canvas-scroll"
          className="scroll-thin min-h-0 w-full flex-1 overflow-y-auto overflow-x-auto"
          style={editing ? DOT_GRID : undefined}
        >
          <GridLayout
            width={gridWidth}
            layout={laid}
            className={GHOST}
            gridConfig={{
              cols: GRID_COLS,
              rowHeight,
              margin: MARGIN,
              containerPadding: PADDING,
            }}
            // Free placement: a dropped tile stays where it was dropped. The
            // default vertical compactor would yank every tile to the top and
            // make "put the diff next to the terminal" impossible.
            compactor={noCompactor}
            dragConfig={{ enabled: editing, handle: "[data-drag-handle]", cancel: "[data-no-drag]" }}
            resizeConfig={{ enabled: editing, handles: ["se"] }}
            onDragStart={track}
            onDrag={track}
            onDragStop={commit}
            onResizeStart={track}
            onResize={track}
            onResizeStop={commit}
          >
            {visible.map((w) => {
              const id = w.widget as WidgetId;
              const def = RUN_WIDGETS[id];
              return (
                <div
                  key={id}
                  className="flex min-h-0 flex-col overflow-hidden rounded-lg"
                  style={editing ? undefined : STILL}
                  // Load-bearing for e2e: the terminal's tile must be findable
                  // AND above the fold. Both presets place it at y=0.
                  data-testid={id === "terminal" ? "run-terminal-pane" : undefined}
                  data-widget={id}
                >
                  {editing && (
                    <TileHandle
                      // The pane calls itself Output on an autonomous run; the handle says the same (#2928).
                      def={id === "terminal" && !ctx.run.interactive ? { ...def, label: RUN_COCKPIT.outputPane } : def}
                      size={active?.i === id ? sizeLabel(active.w, active.h) : undefined}
                      onRemove={def.required ? undefined : () => toggleWidget(id)}
                    />
                  )}
                  {/* The terminal is mounted here as before. Any other widget is rendered once into its host (the
                      portals above) and this box takes the host in; while the widget is expanded the dialog's box has
                      it and the tile is empty. WidgetSlot (the FILL stretch and the per-widget boundary, shared with
                      focus mode's dock) is inside the host. */}
                  {id === "terminal" ? (
                    <WidgetSlot region={def.label} resetKey={ctx.run.id}>
                      {def.component(slotCtx)}
                    </WidgetSlot>
                  ) : (
                    expandedId !== id && <WidgetBox host={hostOf(id)} />
                  )}
                </div>
              );
            })}
          </GridLayout>
        </div>

        {/* The controls get a strip of their own under the grid. They used to float over its bottom-right
            corner, which is where the rail's last widget (Identity) ends, and covered its last rows. */}
        <div className={cn("flex h-12 shrink-0 items-center px-4", editing ? "justify-center" : "justify-end")}>
          {editing ? (
            <div className="z-30 flex items-center gap-1.5 rounded-xl border border-border bg-popover p-1.5 shadow-floating">
              <span className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-primary/40 bg-primary/10 px-2.5 text-xs font-medium text-primary">
                <LayoutGrid className="size-3.5" />
                {RUN_COCKPIT.editing}
              </span>
              <span className="h-5 w-px bg-border" />
              <div role="group" aria-label={RUN_COCKPIT.layoutPresets} className="flex items-center gap-1.5">
                {(["live", "finished"] as const).map((p) => (
                  <button
                    key={p}
                    type="button"
                    aria-pressed={p === preset}
                    onClick={() => setPreset(p)}
                    className={cn(
                      "inline-flex h-8 items-center rounded-lg px-2.5 font-mono text-xs",
                      p === preset
                        ? "border border-border bg-card text-foreground"
                        : "text-muted-foreground hover:text-foreground",
                    )}
                  >
                    {RUN_COCKPIT.layoutPreset(p === "live" ? "Live" : "Finished")}
                  </button>
                ))}
              </div>
              <span className="h-5 w-px bg-border" />
              {/* The catalog offers exactly what a tile could appear for. Deriving
                  it from the whole table while the GRID derived from `renderable`
                  made "Attach from your terminal" a dead control on every finished
                  run and every run you did not start: the click ticked the row, ran
                  addWidget and PUT the phantom placement, and no tile ever came
                  back. Two gates, one of them uninformed. */}
              <Catalog
                open={catalogOpen}
                onOpenChange={setCatalogOpen}
                ids={WIDGET_IDS.filter(renderable)}
                placed={layout.map((w) => w.widget)}
                onToggle={toggleWidget}
              />
              <ToolbarButton onClick={reset} Icon={RotateCcw}>
                {RUN_COCKPIT.resetLayout}
              </ToolbarButton>
              <LayoutStatus status={status} failedPreset={failedPreset} onRetry={retry} />
              <button
                type="button"
                onClick={() => setEditing(false)}
                className="inline-flex h-8 items-center gap-1.5 rounded-lg bg-primary px-3 text-xs font-medium text-primary-foreground"
              >
                <Check className="size-3.5" />
                {RUN_COCKPIT.doneEditing}
              </button>
              {!persistable && (
                <span className="max-w-[16rem] px-1.5 text-meta leading-tight text-muted-foreground">
                  {RUN_COCKPIT.layoutNotPersisted}
                </span>
              )}
            </div>
          ) : (
            // The board puts these controls in the tabs row; that row belongs to
            // run-detail-command-bar.tsx, which this file does not own — so the
            // canvas carries its own entry points.
            <div className="flex items-center gap-1.5">
              {/* A write that failed outlives Done: leaving the editor must not read as "saved". */}
              {status === "failed" && <LayoutStatus status={status} failedPreset={failedPreset} onRetry={retry} />}
              <button
                ref={focusTriggerRef}
                type="button"
                onClick={() => setFocus(true)}
                className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-popover px-2.5 text-xs font-medium text-muted-foreground shadow-xs hover:text-foreground"
              >
                <Expand className="size-3.5" />
                {RUN_COCKPIT.enterFocus}
              </button>
              <button
                type="button"
                onClick={() => setEditing(true)}
                className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-popover px-2.5 text-xs font-medium text-muted-foreground shadow-xs hover:text-foreground"
              >
                <Pencil className="size-3.5" />
                {RUN_COCKPIT.editLayout}
              </button>
            </div>
          )}
        </div>
      </div>

      {/* #3940: the expanded widget, the same live instance moved here from its tile. */}
      <Dialog open={expandedId !== null} onOpenChange={(open) => !open && setExpanded(null)}>
        {expandedId && (
          <DialogContent
            ref={setExpandDialog}
            aria-describedby={undefined}
            className={EXPAND_DIALOG}
            // Radix decides "inside" from the React tree, and the widget's React parent is the canvas, so every click in
            // it would read as outside and close the dialog. Only the overlay dismisses by pointer.
            onPointerDownOutside={(event) => {
              if (!(event.target instanceof Element && event.target.closest('[data-slot="dialog-overlay"]'))) event.preventDefault();
            }}
            // Land on the card's body, not on a button in it: arrow and page keys scroll at once, and nothing is
            // pressed by accident.
            onOpenAutoFocus={(event) => {
              event.preventDefault();
              const content = event.currentTarget as HTMLElement | null;
              (content?.querySelector<HTMLElement>("[data-widget-body]") ?? content)?.focus();
            }}
            // Explicit: Safari does not focus a clicked button, so the opener cannot be trusted to be remembered.
            onCloseAutoFocus={(event) => {
              event.preventDefault();
              (
                document.querySelector<HTMLElement>(`[data-widget="${expandedId}"] [data-widget-expand]`) ??
                document.getElementById("main-content")
              )?.focus();
            }}
          >
            <DialogTitle className="sr-only">{RUN_WIDGETS[expandedId].label}</DialogTitle>
            <WidgetBox host={hostOf(expandedId)} />
            {/* The approvals that hold the run stay reachable: the same persistent host focus mode uses. */}
            <div className="max-h-[40dvh] shrink-0 overflow-y-auto">
              <TerminalSlot host={decisionHost} className="shrink-0" />
            </div>
          </DialogContent>
        )}
      </Dialog>
    </>
  );
}

// The drag handle. WidgetCard takes `dragHandleProps` for exactly this, but the
// five widget files call WidgetCard themselves and none of them forwards such a
// prop — and they are not this lane's to edit — so the canvas puts the handle
// in a strip of its own above the card. It exists only in edit mode, which also
// makes "which tiles can I move" answerable at a glance.
// The Expand control is the opposite case: it belongs in the card's own header, so WidgetCard draws it, from the
// WidgetExpandContext the portals above provide.
function TileHandle({
  def,
  size,
  onRemove,
}: {
  def: { label: string; Icon: React.ElementType };
  size?: string;
  onRemove?: () => void;
}) {
  const Icon = def.Icon;
  return (
    <div
      data-drag-handle
      className="flex h-6 shrink-0 cursor-grab items-center gap-1.5 rounded-t-lg border border-b-0 border-primary/40 bg-primary/10 px-2 active:cursor-grabbing"
    >
      <GripVertical className="size-3 shrink-0 text-primary" aria-hidden />
      <Icon className="size-3 shrink-0 text-primary" aria-hidden />
      <span className="truncate text-meta font-medium text-primary">{def.label}</span>
      <span className="ml-auto flex shrink-0 items-center gap-1">
        {size && (
          <span className="rounded bg-primary px-1 font-mono text-meta font-semibold text-primary-foreground">
            {size}
          </span>
        )}
        {onRemove && (
          <button
            type="button"
            data-no-drag
            onClick={onRemove}
            aria-label={RUN_COCKPIT.removeWidget(def.label)}
            className="rounded p-0.5 text-primary hover:bg-primary/20"
          >
            <X className="size-3" />
          </button>
        )}
      </span>
    </div>
  );
}

function ToolbarButton({
  onClick,
  Icon,
  children,
}: {
  onClick: () => void;
  Icon?: React.ElementType;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="inline-flex h-8 items-center gap-1.5 rounded-lg px-2.5 text-xs text-muted-foreground hover:text-foreground"
    >
      {Icon && <Icon className="size-3.5" />}
      {children}
    </button>
  );
}

// Where the automatic save stands. Silent before the first gesture; a failure names itself and offers the retry.
function LayoutStatus({ status, failedPreset, onRetry }: { status: SaveStatus; failedPreset: RunLayoutPreset | null; onRetry: () => void }) {
  if (status === "idle") return null;
  if (status === "failed") {
    return (
      <span role="status" className="inline-flex items-center gap-1 px-1.5 text-xs text-danger">
        {RUN_COCKPIT.layoutSaveFailed(failedPreset === "finished" ? "Finished" : "Live")}
        <button type="button" onClick={onRetry} className="rounded px-1 font-medium underline underline-offset-2">
          {RUN_COCKPIT.layoutRetry}
        </button>
      </span>
    );
  }
  return (
    <span role="status" className="px-1.5 text-xs text-muted-foreground">
      {status === "saving" ? RUN_COCKPIT.layoutSaving : RUN_COCKPIT.layoutSaved}
    </span>
  );
}

// The catalog. A check means already placed — clicking it takes the widget back
// off the canvas. The terminal's row is inert on purpose: it is the hero, and a
// cockpit with no session in it is not a cockpit.
function Catalog({
  open,
  onOpenChange,
  placed,
  ids,
  onToggle,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The renderable ids, already filtered by the canvas's `renderable` gate —
   *  never WIDGET_IDS. Offering an id whose tile cannot appear is a control
   *  that persists a placement and changes nothing on screen. */
  ids: WidgetId[];
  placed: string[];
  onToggle: (id: WidgetId) => void;
}) {
  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="inline-flex h-8 items-center gap-1.5 rounded-lg px-2.5 text-xs text-muted-foreground hover:text-foreground"
        >
          <Plus className="size-3.5" />
          {RUN_COCKPIT.addWidget}
        </button>
      </PopoverTrigger>
      <PopoverContent align="center" side="top" className="w-72 p-1.5">
        <div className="px-2 py-1.5">
          <div className="label-eyebrow">{RUN_COCKPIT.addWidget}</div>
        </div>
        <div className="flex flex-col">
          {ids.map((id) => {
            const def = RUN_WIDGETS[id];
            const Icon = def.Icon;
            const on = placed.includes(id);
            return (
              <button
                key={id}
                type="button"
                aria-pressed={on}
                disabled={def.required}
                onClick={() => onToggle(id)}
                className={cn(
                  "flex items-center gap-2 rounded-md px-2 py-1.5 text-sm",
                  def.required ? "text-muted-foreground" : "hover:bg-accent",
                  on ? "text-foreground" : "text-muted-foreground",
                )}
              >
                <Icon className="size-3.5 shrink-0" aria-hidden />
                <span className="min-w-0 flex-1 truncate text-left">{def.label}</span>
                {on && <Check className="size-3.5 shrink-0 text-primary" aria-hidden />}
              </button>
            );
          })}
        </div>
      </PopoverContent>
    </Popover>
  );
}
