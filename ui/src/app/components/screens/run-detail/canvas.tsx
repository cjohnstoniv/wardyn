/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RUN CANVAS — design board 2b. What was a hard-coded 400px evidence rail is
// now a free 12-column grid the operator arranges: tiles anywhere, drag from a
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
//   - the terminal cannot be arranged away — see widget-registry's `required`.
//
// Grid engine: react-grid-layout v2 (`noCompactor`, so a tile stays where it
// was dropped instead of being sucked upward). Nothing already installed does
// free x/y placement with collision, a snap ghost and resize handles, and that
// is several hundred lines of pointer math to hand-roll badly.
import * as React from "react";
import GridLayout, { noCompactor, type Layout } from "react-grid-layout";
import "react-grid-layout/css/styles.css";
import { Check, Expand, GripVertical, LayoutGrid, Pencil, Plus, RotateCcw, X } from "lucide-react";
import { toast } from "sonner";
import type { RunLayoutPreset } from "../../../lib/api/run-layout";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import { cn } from "../../ui/utils";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { useFocusMode } from "../app-shell";
import { FocusMode } from "./focus-mode";
import { useRunLayout } from "./use-run-layout";
import {
  GRID_COLS,
  GRID_ROWS,
  RUN_WIDGETS,
  WIDGET_IDS,
  addWidget,
  removeWidget,
  type WidgetContext,
  type WidgetId,
} from "./widget-registry";

const MARGIN: [number, number] = [10, 10];
const PADDING: [number, number] = [10, 10];

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

// Make any widget fill its tile without touching the five widget files (they
// are not this lane's to change, and none of them forwards a className):
// a WidgetCard renders a <section>, so stretch that, and give its BODY — the
// section's last child — the scroll it needs when a tile is smaller than its
// contents. Clipping evidence silently is the one thing the rail this replaces
// was explicitly built not to do.
const FILL_TILE = [
  "[&>section]:min-h-0 [&>section]:flex-1",
  "[&>section>*:last-child]:min-h-0 [&>section>*:last-child]:flex-1",
  "[&>section>*:last-child]:overflow-y-auto",
].join(" ");

/** Measures the scroll box: width for the grid, height to size a row.
 *  A callback ref rather than useRef, because focus mode unmounts this box and
 *  brings it back: a `[]` effect would keep observing the DETACHED node and
 *  every row height after the first exit would be stale. */
function useCanvasSize() {
  const [node, setNode] = React.useState<HTMLDivElement | null>(null);
  const [size, setSize] = React.useState({ width: 0, height: 0 });
  React.useEffect(() => {
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
  return { ref: setNode, width: size.width, height: size.height };
}

export function RunCanvas({ ctx }: { ctx: WidgetContext }) {
  // The situation picks the preset; the toolbar can override it while editing.
  // Interactive vs autonomous is NOT a third preset — that changes the Terminal
  // widget's own state, not the arrangement.
  const situational: RunLayoutPreset = ctx.finished ? "finished" : "live";
  const [preset, setPreset] = React.useState<RunLayoutPreset>(situational);
  React.useEffect(() => setPreset(situational), [situational]);

  const [editing, setEditing] = React.useState(false);
  const [catalogOpen, setCatalogOpen] = React.useState(false);
  // The tile under the cursor, for the size readout. Only updated when its
  // dimensions actually change, so a drag is not a re-render per frame.
  const [active, setActive] = React.useState<{ i: string; w: number; h: number } | null>(null);

  const { layout, persistable, apply, save, reset } = useRunLayout(preset);
  const { ref, width, height } = useCanvasSize();

  // FOCUS MODE (design board 2c). The shell learns about it through
  // app-shell's FocusContext, which is also why this is safe outside the
  // console: a canvas mounted with no <AppShell> above it gets the default
  // no-op setter and simply never enters focus.
  const { focus, setFocus } = useFocusMode();
  const exitFocus = React.useCallback(() => setFocus(false), [setFocus]);
  // Never leave the console headless: whatever unmounts this canvas — a route
  // change, an ErrorBoundary catch — puts the shell's chrome back.
  React.useEffect(() => () => setFocus(false), [setFocus]);

  // A row is one twelfth of the visible pane, so a preset that adds up to
  // GRID_ROWS fills the cockpit exactly and nothing scrolls. Floors keep the
  // arithmetic sane before the first measurement (and under jsdom).
  const gridWidth = Math.max(width, 480);
  const rowHeight = Math.max(
    28,
    Math.round((Math.max(height, 520) - 2 * PADDING[1] - (GRID_ROWS - 1) * MARGIN[1]) / GRID_ROWS),
  );
  const colWidth = (gridWidth - 2 * PADDING[0] - (GRID_COLS - 1) * MARGIN[0]) / GRID_COLS;
  const sizeLabel = (w: number, h: number) =>
    `${Math.round(w * colWidth + (w - 1) * MARGIN[0])} × ${Math.round(h * rowHeight + (h - 1) * MARGIN[1])}`;

  // A widget can be placed but not renderable right now (ssh on a run you do
  // not own). Keep it OUT of the grid — an empty tile over a dot grid is worse
  // than nothing — but keep its saved placement, or merely viewing someone
  // else's run would quietly delete it.
  const visible = layout.filter((w) => RUN_WIDGETS[w.widget as WidgetId].available?.(ctx) ?? true);
  const hidden = layout.filter((w) => !visible.includes(w));

  const items: Layout = visible.map((w) => ({
    i: w.widget,
    x: w.x,
    y: w.y,
    w: w.w,
    h: w.h,
    minW: RUN_WIDGETS[w.widget as WidgetId].defaultLayout.minW,
    minH: RUN_WIDGETS[w.widget as WidgetId].defaultLayout.minH,
  }));

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

  const saveDefault = () => {
    void save().then((ok) =>
      ok ? toast.success(RUN_COCKPIT.layoutSaved) : toast.message(RUN_COCKPIT.layoutNotPersisted),
    );
  };

  // The terminal moves OUT of the grid and into the overlay, so it remounts and
  // the attach socket reconnects — the tmux session survives that (it survives
  // a full page refresh), which is exactly why the terminal can be moved at
  // all. Rendering it in both places at once would open two sockets.
  if (focus) return <FocusMode ctx={ctx} onExit={exitFocus} />;

  return (
    <div className="relative min-h-0 flex-1 overflow-hidden">
      <div
        ref={ref}
        className="scroll-thin h-full w-full overflow-y-auto overflow-x-hidden"
        style={editing ? DOT_GRID : undefined}
      >
        <GridLayout
          width={gridWidth}
          layout={items}
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
                // Load-bearing for e2e: the terminal's tile must be findable
                // AND above the fold. Both presets place it at y=0.
                data-testid={id === "terminal" ? "run-terminal-pane" : undefined}
              >
                {editing && (
                  <TileHandle
                    def={def}
                    size={active?.i === id ? sizeLabel(active.w, active.h) : undefined}
                    onRemove={def.required ? undefined : () => toggleWidget(id)}
                  />
                )}
                <div className={cn("flex min-h-0 flex-1 flex-col", FILL_TILE)}>{def.component(ctx)}</div>
              </div>
            );
          })}
        </GridLayout>
      </div>

      {editing ? (
        <div className="absolute bottom-4 left-1/2 z-30 flex -translate-x-1/2 items-center gap-1.5 rounded-xl border border-border bg-popover p-1.5 shadow-lg">
          <span className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-primary/40 bg-primary/10 px-2.5 text-xs font-medium text-primary">
            <LayoutGrid className="size-3.5" />
            {RUN_COCKPIT.editing}
          </span>
          <span className="h-5 w-px bg-border" />
          {(["live", "finished"] as const).map((p) => (
            <button
              key={p}
              type="button"
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
          <span className="h-5 w-px bg-border" />
          <Catalog
            open={catalogOpen}
            onOpenChange={setCatalogOpen}
            placed={layout.map((w) => w.widget)}
            onToggle={toggleWidget}
          />
          <ToolbarButton onClick={reset} Icon={RotateCcw}>
            {RUN_COCKPIT.resetLayout}
          </ToolbarButton>
          <ToolbarButton onClick={saveDefault}>{RUN_COCKPIT.saveLayoutDefault}</ToolbarButton>
          <button
            type="button"
            onClick={() => setEditing(false)}
            className="inline-flex h-8 items-center gap-1.5 rounded-lg bg-primary px-3 text-xs font-medium text-primary-foreground"
          >
            <Check className="size-3.5" />
            {RUN_COCKPIT.doneEditing}
          </button>
          {!persistable && (
            <span className="max-w-[16rem] px-1.5 text-[0.6875rem] leading-tight text-muted-foreground">
              {RUN_COCKPIT.layoutNotPersisted}
            </span>
          )}
        </div>
      ) : (
        // The board puts these controls in the tabs row; that row belongs to
        // run-detail-command-bar.tsx, which this lane does not own — so the
        // canvas carries its own entry points (see the hand-back).
        <div className="absolute bottom-4 right-4 z-30 flex items-center gap-1.5">
          <button
            type="button"
            onClick={() => setFocus(true)}
            className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-popover px-2.5 text-xs font-medium text-muted-foreground shadow-lg hover:text-foreground"
          >
            <Expand className="size-3.5" />
            {RUN_COCKPIT.enterFocus}
          </button>
          <button
            type="button"
            onClick={() => setEditing(true)}
            className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-popover px-2.5 text-xs font-medium text-muted-foreground shadow-lg hover:text-foreground"
          >
            <Pencil className="size-3.5" />
            {RUN_COCKPIT.editLayout}
          </button>
        </div>
      )}
    </div>
  );
}

// The drag handle. WidgetCard takes `dragHandleProps` for exactly this, but the
// five widget files call WidgetCard themselves and none of them forwards such a
// prop — and they are not this lane's to edit — so the canvas puts the handle
// in a strip of its own above the card. It exists only in edit mode, which also
// makes "which tiles can I move" answerable at a glance.
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
      <span className="truncate text-[0.6875rem] font-medium text-primary">{def.label}</span>
      <span className="ml-auto flex shrink-0 items-center gap-1">
        {size && (
          <span className="rounded bg-primary px-1 font-mono text-[0.625rem] font-semibold text-primary-foreground">
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

// The catalog. A check means already placed — clicking it takes the widget back
// off the canvas. The terminal's row is inert on purpose: it is the hero, and a
// cockpit with no session in it is not a cockpit.
function Catalog({
  open,
  onOpenChange,
  placed,
  onToggle,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
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
          {WIDGET_IDS.map((id) => {
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
