/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// TierPicker (#1200) — the compact barrier-tier control: one row per VISIBLE
// tier (readiness dot, name, one-line strength), an info popover per row for
// the mechanism/isolates/doesn't-stop detail, and a "Compare barriers" dialog
// holding the full matrix. Two shapes (segmented / dropdown under ~360px) and
// two modes:
//
//   picker  — interactive (New Run, a member's own choice). Collapses to a
//             single DECIDED row with no control when exactly one tier is
//             visible, and to a danger card naming the requirement when none
//             is (T-9: floor and host disagree) — member Getting-started's
//             summary reaches for exactly that empty-tiers branch (tiers=[])
//             to render the SAME requirement card when its own governance
//             floor blocks every installed tier, never a hover-only tooltip.
//   display — read-only (Settings' Host card only). Every tier handed to it
//             renders as a decided row — there is nothing to pick, only to
//             state.
//
// "Visible" is the CALLER's decision, not this component's: pass `tiers`
// already filtered to installed (Host card, T-10) or installed ∧ allowed
// (everywhere a person is bound by a governance floor). Nothing here talks to
// /setup/status or /policies/default — see visibleTiers()/allowedFromFloor()
// below for the pure intersection math every caller shares.
//
// The full Fence/Wall/Vault matrix stays on environment-step.tsx (Getting
// started, admin setup) — untouched, and not reused as markup here: this
// component's own Compare-barriers dialog is a second, STATIC renderer of the
// same CC_META/CC_MATRIX_ROWS/CC_MATRIX_WHERE data (never a duplicate copy of
// the wording, only of the table markup a read-only dialog needs).
import * as React from "react";
import { Info, AlertOctagon } from "lucide-react";
import { cn } from "../ui/utils";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "../ui/dialog";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { StatusChip } from "./status-chip";
import { Chip, ConfinementChip } from "./primitives";
import { CC_META, CC_MATRIX_ROWS, CC_MATRIX_WHERE, CONFINEMENT_CONSTANT_NOTE, type CCMark } from "./cc-meta";
import { RESIDUAL_PREFIX } from "./copy";
import { TIER_PICKER } from "../../lib/tier-picker-copy";
import { CC_ORDER, type ConfinementClass } from "../../lib/types";
import { PICK_WHEN } from "../screens/setup/environment-step";

/** Every installed tier that is also allowed. `allowed` undefined/null means
 *  no floor at all — every installed tier is allowed. Order follows CC_ORDER
 *  (weakest first), the ladder every other tier surface teaches. */
export function visibleTiers(
  installed: ConfinementClass[],
  allowed?: ConfinementClass[] | null,
): ConfinementClass[] {
  const allow = allowed ?? CC_ORDER;
  return CC_ORDER.filter((cc) => installed.includes(cc) && allow.includes(cc));
}

/** A governance ceiling's min_confinement_class floor, as the "allowed" list
 *  visibleTiers() takes — the floor and every stronger tier. `null`/`undefined`
 *  (no floor authored) returns null, meaning "no restriction". */
export function allowedFromFloor(floor: ConfinementClass | null | undefined): ConfinementClass[] | null {
  if (!floor || !CC_ORDER.includes(floor)) return null;
  return CC_ORDER.slice(CC_ORDER.indexOf(floor));
}

const NARROW_PX = 360;

/** Pure breakpoint rule (T-1): segmented everywhere there's room, dropdown
 *  only under ~360px. Exported so the rule is checkable without a real
 *  ResizeObserver (jsdom's is a no-op stub — see ui/src/test/setup.ts). */
export function pickerShape(widthPx: number): "segmented" | "dropdown" {
  return widthPx > 0 && widthPx < NARROW_PX ? "dropdown" : "segmented";
}

/** Tracks the wrapper's own content width via ResizeObserver, when the
 *  environment has one — a stub (no callback ever fires) leaves `width` at 0,
 *  which pickerShape reads as "segmented", the safe default. */
function useElementWidth<T extends HTMLElement>(): [React.RefObject<T>, number] {
  const ref = React.useRef<T>(null);
  const [width, setWidth] = React.useState(0);
  React.useEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver((entries) => {
      const w = entries[0]?.contentRect.width;
      if (typeof w === "number") setWidth(w);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width];
}

export interface TierPickerProps {
  /** The tiers to present — already filtered by the caller (visibleTiers). */
  tiers: ConfinementClass[];
  /** picker (default): interactive, collapses to decided/requirement at the
   *  edges. display: every tier renders as a read-only decided row. */
  mode?: "picker" | "display";
  /** picker mode only. */
  selected?: ConfinementClass | null;
  onSelect?: (cc: ConfinementClass) => void;
  /** Badges one row "Recommended". */
  recommended?: ConfinementClass | null;
  /** Force a shape; omitted sizes responsively (segmented, dropdown <360px). */
  shape?: "segmented" | "dropdown";
  /** picker mode, zero tiers visible (T-9): names what's missing, e.g.
   *  `TIER_PICKER.REQUIREMENT_LINE(CC_META.CC3.label, kvmReason)`. */
  requirementNote?: React.ReactNode;
  /** picker mode, exactly one tier visible: the line that replaces its
   *  tagline. Defaults to `TIER_PICKER.DECIDED` ("{Tier} · set by your
   *  admin") — the CALLER decides this, not this component (#1200 review
   *  P2-1): that line is a claim about WHY only one tier is left, true only
   *  when a governance floor actually removed one. A caller whose single
   *  survivor is for some other reason (the host itself has only one tier
   *  installed, or the run's OWN authored policy floor) must pass a neutral
   *  line instead — e.g. `RUN.BARRIER_ONLY_QUALIFIER`. */
  decidedLine?: (tierLabel: string) => string;
  /** The instruction line under a real (2+-tier) choice. Defaults to
   *  `TIER_PICKER.PICK_ONE`, which claims the pick "saves it in this browser
   *  as the default barrier for new runs". No surface persists a pick
   *  (default-confinement.ts), so a caller must pass an override; New Run
   *  passes PICK_ONE_PER_RUN (#1200 review P2-3). */
  pickOneNote?: React.ReactNode;
  /** The host probe did not answer (#1238): the rows are the caller's
   *  fallback set, not tiers this host was seen to build, so each carries the
   *  neutral "Unverified" chip instead of "Ready". Default false — every other
   *  caller hands over tiers it already knows are installed. */
  unprobed?: boolean;
  className?: string;
}

export function TierPicker({
  tiers,
  mode = "picker",
  selected = null,
  onSelect,
  recommended = null,
  shape,
  requirementNote,
  decidedLine,
  pickOneNote,
  unprobed,
  className,
}: TierPickerProps) {
  const [ref, measuredWidth] = useElementWidth<HTMLDivElement>();
  const resolvedShape = shape ?? pickerShape(measuredWidth);
  const [compareOpen, setCompareOpen] = React.useState(false);

  const compareLink = (
    <button
      type="button"
      onClick={() => setCompareOpen(true)}
      className="mt-2 text-xs font-medium text-primary hover:underline"
    >
      {TIER_PICKER.COMPARE_BARRIERS} &rarr;
    </button>
  );

  if (mode === "picker" && tiers.length === 0) {
    return (
      <div ref={ref} className={className}>
        <div className="rounded-xl border border-danger/40 bg-danger-subtle p-4">
          <div className="flex items-start gap-3">
            <AlertOctagon className="mt-0.5 size-5 text-danger" aria-hidden />
            {/* Reuses the same danger-card severity every no-runner/incompatible
                state already renders (T-9) — the LINE itself is the caller's:
                the frozen no-runner sentence when nothing was installed at
                all, or the specific "your admin requires X, and this host
                can't run it" line when a runner exists but not the required
                tier. Never both at once — one honest cause, one sentence. */}
            <div className="text-foreground">{requirementNote ?? TIER_PICKER.REQUIREMENT_TITLE}</div>
          </div>
        </div>
        {compareLink}
        <CompareBarriersDialog open={compareOpen} onOpenChange={setCompareOpen} />
      </div>
    );
  }

  if (tiers.length === 0) {
    return (
      <div ref={ref} className={className}>
        <p className="text-sm text-muted-foreground">{TIER_PICKER.NONE_INSTALLED}</p>
        {compareLink}
        <CompareBarriersDialog open={compareOpen} onOpenChange={setCompareOpen} />
      </div>
    );
  }

  // picker mode, exactly one tier: DECIDED — no control at all (T-1/general
  // rule). display mode always renders every row this way (nothing to pick).
  if (mode === "display" || tiers.length === 1) {
    return (
      <div ref={ref} className={className}>
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          {tiers.map((cc, i) => (
            <TierRow
              key={cc}
              cc={cc}
              decided={mode === "picker" ? (decidedLine ?? TIER_PICKER.DECIDED)(CC_META[cc].label) : undefined}
              last={i === tiers.length - 1}
              recommended={mode === "display" && recommended === cc}
              unprobed={unprobed}
            />
          ))}
        </div>
        {compareLink}
        <CompareBarriersDialog open={compareOpen} onOpenChange={setCompareOpen} />
      </div>
    );
  }

  // Two or more visible tiers: a real choice.
  return (
    <div ref={ref} className={className}>
      {resolvedShape === "dropdown" ? (
        <DropdownPicker tiers={tiers} selected={selected} onSelect={onSelect} recommended={recommended} />
      ) : (
        <div role="radiogroup" aria-label="Barrier tier" className="overflow-hidden rounded-xl border border-border bg-card">
          {tiers.map((cc, i) => (
            <TierRow
              key={cc}
              cc={cc}
              selectable
              selected={selected === cc}
              onSelect={() => onSelect?.(cc)}
              last={i === tiers.length - 1}
              recommended={recommended === cc}
              unprobed={unprobed}
            />
          ))}
        </div>
      )}
      <p className="mt-2 text-xs text-muted-foreground">{pickOneNote ?? TIER_PICKER.PICK_ONE}</p>
      {compareLink}
      <CompareBarriersDialog open={compareOpen} onOpenChange={setCompareOpen} />
    </div>
  );
}

function DropdownPicker({
  tiers,
  selected,
  onSelect,
  recommended,
}: {
  tiers: ConfinementClass[];
  selected: ConfinementClass | null;
  onSelect?: (cc: ConfinementClass) => void;
  recommended: ConfinementClass | null;
}) {
  const current = selected && tiers.includes(selected) ? selected : tiers[0];
  return (
    <Select value={current} onValueChange={(v) => onSelect?.(v as ConfinementClass)}>
      <SelectTrigger aria-label="Barrier tier">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {tiers.map((cc) => (
          <SelectItem key={cc} value={cc}>
            <span className="flex items-center gap-2">
              {CC_META[cc].label}
              <span className="text-xs text-muted-foreground">{CC_META[cc].tagline}</span>
              {recommended === cc && <Chip tone="neutral">Recommended</Chip>}
            </span>
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

// One row: a readiness dot (every visible tier is installed, so "ready" —
// unless the caller says the host was never probed, then "unverified"), the tier's own ConfinementChip, its one-line strength, an
// optional Recommended chip, and the info popover. `decided` renders the
// frozen "{Tier} · set by your admin" line instead of the tagline, and drops
// the radio semantics entirely — there is nothing to pick.
function TierRow({
  cc,
  selectable,
  selected,
  decided,
  recommended,
  last,
  unprobed,
  onSelect,
}: {
  cc: ConfinementClass;
  selectable?: boolean;
  selected?: boolean;
  decided?: string;
  recommended?: boolean;
  last?: boolean;
  unprobed?: boolean;
  onSelect?: () => void;
}) {
  const meta = CC_META[cc];
  const content = (
    <>
      <StatusChip status={unprobed ? "unverified" : "ready"} />
      <ConfinementChip value={cc} />
      <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">
        {decided ?? meta.tagline}
      </span>
      {recommended && <Chip tone="neutral">Recommended</Chip>}
      <TierInfoPopover cc={cc} />
    </>
  );
  const rowClass = cn(
    "flex items-center gap-2 px-3 py-2.5",
    !last && "border-b border-border",
    decided && "bg-muted/40",
  );
  if (!selectable) {
    return (
      <div className={rowClass} role="status">
        {content}
      </div>
    );
  }
  return (
    <div
      role="radio"
      aria-checked={!!selected}
      aria-label={meta.label}
      tabIndex={0}
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect?.();
        }
      }}
      className={cn(rowClass, "cursor-pointer", selected && "bg-primary/5 shadow-[inset_3px_0_0_0_var(--primary)]")}
    >
      {content}
    </div>
  );
}

function TierInfoPopover({ cc }: { cc: ConfinementClass }) {
  const meta = CC_META[cc];
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={TIER_PICKER.ABOUT(meta.label)}
          onClick={(e) => e.stopPropagation()}
          className="flex size-5 shrink-0 items-center justify-center rounded-full border border-border-strong text-muted-foreground hover:border-foreground hover:text-foreground"
        >
          <Info className="size-3" aria-hidden />
        </button>
      </PopoverTrigger>
      <PopoverContent onClick={(e) => e.stopPropagation()} className="w-72 text-xs">
        <h5 className="text-sm font-medium text-foreground">{meta.label}</h5>
        <p className="mt-1.5 text-muted-foreground">{meta.mechanism}</p>
        <p className="mt-1.5 text-muted-foreground">
          <span className="font-medium text-foreground">Isolates:</span> {meta.protects}
        </p>
        <p className="mt-1.5 text-warning">
          {RESIDUAL_PREFIX} {meta.doesntProtect}
        </p>
      </PopoverContent>
    </Popover>
  );
}

// Compare-barriers dialog (T-3): the full matrix, read-only, shared by every
// TierPicker instance — never the interactive radiogroup environment-step.tsx
// renders, only the data it already exports.
function CompareBarriersDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{TIER_PICKER.COMPARE_BARRIERS_TITLE}</DialogTitle>
        </DialogHeader>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] border-collapse text-sm">
            <thead>
              <tr>
                <th className="border-b px-3 py-2 text-left align-top font-normal" />
                {CC_ORDER.map((cc) => (
                  <th key={cc} className="border-b px-3 py-2 text-left align-top font-normal">
                    <span className="text-sm font-medium text-foreground">{CC_META[cc].label}</span>{" "}
                    <span className="text-xs text-muted-foreground">{cc}</span>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              <MatrixRow label="Strength">{(cc) => CC_META[cc].tagline}</MatrixRow>
              <MatrixRow label="Mechanism">{(cc) => CC_META[cc].mechanism}</MatrixRow>
              {CC_MATRIX_ROWS.map((row) => (
                <tr key={row.label}>
                  <th scope="row" className="border-b px-3 py-2 text-left align-top font-normal text-foreground">
                    {row.label}
                  </th>
                  {CC_ORDER.map((cc) => (
                    <td key={cc} className="border-b px-3 py-2 text-center align-top">
                      <MarkCell mark={row.cells[cc]} cc={cc} />
                    </td>
                  ))}
                </tr>
              ))}
              <MatrixRow label={<span className="text-warning">{RESIDUAL_PREFIX}</span>}>
                {(cc) => CC_META[cc].doesntProtect}
              </MatrixRow>
              {/* #1200 review P1-2 — the full table's own "Pick when" row
                  (environment-step.tsx's PICK_WHEN), missing from this
                  dialog's first cut. */}
              <MatrixRow label="Pick when">{(cc) => PICK_WHEN[cc]}</MatrixRow>
              <tr>
                <th scope="row" className="px-3 py-2 text-left align-top font-normal text-foreground">
                  {CC_MATRIX_WHERE.label}
                </th>
                {CC_ORDER.map((cc) => (
                  <td key={cc} className="px-3 py-2 text-xs text-muted-foreground">
                    {CC_MATRIX_WHERE.cells[cc]}
                  </td>
                ))}
              </tr>
            </tbody>
          </table>
        </div>
        <p className="text-sm text-muted-foreground">{CONFINEMENT_CONSTANT_NOTE}</p>
        <a
          href={TIER_PICKER.READ_THE_DOCS_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="text-sm font-medium text-primary hover:underline"
        >
          {TIER_PICKER.READ_THE_DOCS} &rarr;
        </a>
      </DialogContent>
    </Dialog>
  );
}

function MatrixRow({
  label,
  children,
}: {
  label: React.ReactNode;
  children: (cc: ConfinementClass) => React.ReactNode;
}) {
  return (
    <tr>
      <th scope="row" className="border-b px-3 py-2 text-left align-top font-normal text-foreground">
        {label}
      </th>
      {CC_ORDER.map((cc) => (
        <td key={cc} className="border-b px-3 py-2 align-top text-muted-foreground">
          {children(cc)}
        </td>
      ))}
    </tr>
  );
}

function MarkCell({ mark, cc }: { mark: CCMark; cc: ConfinementClass }) {
  if (mark === "yes") return <span className="text-success">&#10003;</span>;
  if (mark === "no") return <span className="text-danger">&#10007;</span>;
  return (
    <span title={`${RESIDUAL_PREFIX} ${CC_META[cc].doesntProtect}`} className="text-warning">
      &#9888;
    </span>
  );
}
