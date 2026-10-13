/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The barrier comparison control: ONE radiogroup whose radios sit in the column
// headers. From 521 px it renders as a real <table> (column and row headers);
// below that as one self-labelled card per barrier. Both layouts are in the markup and
// CSS display shows exactly one, so the hidden set is out of the accessibility
// tree and the page never has two live radio sets.
import * as React from "react";
import { Info, Lock } from "lucide-react";
import { cn } from "../ui/utils";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { BarrierArt } from "./barrier-art";
import { BarrierStrengthStrip } from "./barrier-strength-strip";
import { CAVEAT_PREFIX, CC_META, CC_MATRIX_ROWS, CC_MATRIX_WHERE } from "./cc-meta";
import { MarkCell } from "./mark-cell";
import { Chip } from "./primitives";
import { TIER_PICKER } from "../../lib/tier-picker-copy";
import type { ConfinementClass } from "../../lib/types";

export interface BarrierOption {
  cc: ConfinementClass;
  available: boolean;
  /** Why the barrier cannot be picked; shown as visible text and described to the radio. */
  reason?: string;
}

// PLACEHOLDER: the info popover's three headings are canon strings of the
// approved mock that copy/new-run/* (E1d) will own; swap this constant for that
// import when the lanes merge.
const INFO_HEADINGS = {
  protects: "Protects against",
  doesntProtect: "Doesn't protect against",
  howItWorks: "How it works",
};

// Make's barrier comparison (snapshot App.tsx:1798-1912, index.css:240-275): a 1 px-ruled grid of the corner,
// three column heads (art, name, tagline, badge, radio dot, lock reason) and the criteria rows, each cell washed
// in its barrier's colour and hatched when the barrier is unavailable. Below 521 px, one card per barrier.
const TINT: Record<ConfinementClass, { text: string; border: string; wash: string; ring: string; dot: string }> = {
  CC1: { text: "text-fence-fg", border: "border-fence-border", wash: "var(--fence-bg)", ring: "shadow-[inset_0_0_0_2px_var(--fence-fg)]", dot: "border-fence-fg after:bg-fence-fg" },
  CC2: { text: "text-wall-fg", border: "border-wall-border", wash: "var(--wall-bg)", ring: "shadow-[inset_0_0_0_2px_var(--wall-fg)]", dot: "border-wall-fg after:bg-wall-fg" },
  CC3: { text: "text-vault-fg", border: "border-vault-border", wash: "var(--vault-bg)", ring: "shadow-[inset_0_0_0_2px_var(--vault-fg)]", dot: "border-vault-fg after:bg-vault-fg" },
};
const HATCH =
  "repeating-linear-gradient(-45deg, transparent, transparent 7px, color-mix(in srgb, var(--border) 35%, transparent) 7px, color-mix(in srgb, var(--border) 35%, transparent) 8px)";
/** The barrier's wash over the cell's own surface, hatched when it cannot be picked. */
const fill = (cc: ConfinementClass, available: boolean): React.CSSProperties => ({
  backgroundImage: [!available && HATCH, `linear-gradient(${TINT[cc].wash}, ${TINT[cc].wash})`].filter(Boolean).join(", "),
});

export function BarrierCompareTable({
  label,
  options,
  value,
  onChange,
  defaultBadge,
  badges,
  showWhere,
}: {
  /** Accessible name of the radiogroup (the field's label). */
  label: string;
  options: BarrierOption[];
  value?: ConfinementClass;
  onChange: (cc: ConfinementClass) => void;
  defaultBadge?: { cc: ConfinementClass; text: string };
  badges?: Partial<Record<ConfinementClass, string>>;
  showWhere: boolean;
}) {
  const uid = React.useId();
  const shared = { uid, value, onChange, defaultBadge, badges };
  // A click anywhere in an available column picks it, as in Make; the radios stay the keyboard path. A click on
  // the radio's own label or the info button (or in its popover, which React bubbles through) is theirs.
  const pick = (o: BarrierOption) => (e: React.MouseEvent<HTMLElement>) => {
    const target = e.target as HTMLElement;
    if (o.available && e.currentTarget.contains(target) && !target.closest("label, button")) onChange(o.cc);
  };
  return (
    <div role="radiogroup" aria-label={label}>
      <div className="hidden overflow-hidden rounded-[10px] bg-border min-[521px]:block">
        <table className="w-full table-fixed border-separate border-spacing-px text-sm">
          <caption className="sr-only">{TIER_PICKER.COMPARE_BARRIERS_TITLE}</caption>
          <colgroup>
            <col className="w-[23%]" />
            {options.map((o) => <col key={o.cc} />)}
          </colgroup>
          <thead>
            <tr>
              <th scope="col" className="bg-card p-4 text-left align-bottom text-sm font-semibold text-muted-foreground">
                {TIER_PICKER.COMPARE_BARRIERS_TITLE}
              </th>
              {options.map((o) => (
                <th key={o.cc} scope="col" className="h-[214px] bg-card p-0 align-top font-normal">
                  <BarrierChoice option={o} instance="table" {...shared} />
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {CC_MATRIX_ROWS.map((row) => (
              <tr key={row.label}>
                <th scope="row" className="bg-card px-4 py-[15px] text-left align-middle text-sm leading-snug font-semibold text-foreground">
                  {row.label}
                </th>
                {options.map((o) => (
                  <td key={o.cc} style={fill(o.cc, o.available)} onClick={pick(o)}
                    className={cn("h-16 bg-card p-3.5 text-center align-middle", o.available ? "cursor-pointer" : "cursor-not-allowed")}>
                    <Mark mark={row.cells[o.cc]} cc={o.cc} />
                  </td>
                ))}
              </tr>
            ))}
            {showWhere && (
              <tr>
                <th scope="row" className="bg-card px-4 py-[15px] text-left align-middle text-sm leading-snug font-semibold text-foreground">
                  {CC_MATRIX_WHERE.label}
                </th>
                {options.map((o) => (
                  <td key={o.cc} style={fill(o.cc, o.available)} onClick={pick(o)}
                    className={cn("h-16 bg-card p-3.5 text-center align-middle text-sm leading-snug font-semibold", o.available ? "cursor-pointer" : "cursor-not-allowed text-muted-foreground")}>
                    {CC_MATRIX_WHERE.cells[o.cc]}
                  </td>
                ))}
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="flex flex-col gap-3 min-[521px]:hidden">
        {options.map((o) => (
          <section key={o.cc} style={fill(o.cc, o.available)} onClick={pick(o)}
            className={cn("overflow-visible rounded-[10px] border", TINT[o.cc].border, o.available ? "cursor-pointer" : "cursor-not-allowed")}>
            <BarrierChoice option={o} instance="card" {...shared} />
            <dl className={cn("border-t", TINT[o.cc].border)}>
              {CC_MATRIX_ROWS.map((row) => (
                <div key={row.label} className={cn("grid min-h-[58px] grid-cols-[minmax(0,1fr)_auto] items-center gap-3 border-b px-3.5 py-3 text-sm leading-snug last:border-b-0", TINT[o.cc].border)}>
                  <dt className="text-foreground">{row.label}</dt>
                  <dd>
                    <Mark mark={row.cells[o.cc]} cc={o.cc} />
                  </dd>
                </div>
              ))}
              {showWhere && (
                <div className={cn("grid min-h-[58px] grid-cols-[minmax(0,1fr)_auto] items-center gap-3 px-3.5 py-3 text-sm leading-snug", TINT[o.cc].border)}>
                  <dt className="text-foreground">{CC_MATRIX_WHERE.label}</dt>
                  <dd className="max-w-[135px] text-right font-semibold">{CC_MATRIX_WHERE.cells[o.cc]}</dd>
                </div>
              )}
            </dl>
          </section>
        ))}
      </div>
    </div>
  );
}

const MARK_DISC = { yes: "bg-success-subtle", no: "bg-danger-subtle", caveat: "bg-warning-subtle" } as const;

// A caveat names its residual risk as text a screen reader reads, not only as a
// hover title.
function Mark({ mark, cc }: { mark: Parameters<typeof MarkCell>[0]["mark"]; cc: ConfinementClass }) {
  return (
    <span className={cn("mx-auto grid size-[26px] place-items-center rounded-full", MARK_DISC[mark])}>
      <MarkCell mark={mark} cc={cc} caveatPrefix={CAVEAT_PREFIX} />
      {mark === "caveat" && (
        <span className="sr-only">
          {CAVEAT_PREFIX} {CC_META[cc].doesntProtect}
        </span>
      )}
    </span>
  );
}

function BarrierChoice({
  option,
  instance,
  uid,
  value,
  onChange,
  defaultBadge,
  badges,
}: {
  option: BarrierOption;
  instance: "table" | "card";
  uid: string;
  value?: ConfinementClass;
  onChange: (cc: ConfinementClass) => void;
  defaultBadge?: { cc: ConfinementClass; text: string };
  badges?: Partial<Record<ConfinementClass, string>>;
}) {
  const { cc, available, reason } = option;
  const meta = CC_META[cc];
  const tint = TINT[cc];
  const selected = value === cc;
  const nameId = `${uid}-${instance}-${cc}-name`;
  const reasonId = `${uid}-${instance}-${cc}-reason`;
  const badge = badges?.[cc];
  return (
    <label
      style={fill(cc, available)}
      className={cn(
        "relative grid min-w-0 content-start justify-items-center gap-2 p-3.5",
        "has-[:focus-visible]:outline-2 has-[:focus-visible]:-outline-offset-2 has-[:focus-visible]:outline-ring",
        instance === "table" && "h-full min-h-[214px] [grid-template-rows:auto_auto_20px_20px_auto]",
        selected && tint.ring,
        available ? "cursor-pointer" : "cursor-not-allowed",
      )}
    >
      <input
        type="radio"
        name={`${uid}-${instance}-barrier`}
        value={cc}
        checked={selected}
        disabled={!available}
        onChange={() => onChange(cc)}
        aria-labelledby={nameId}
        aria-describedby={!available && reason ? reasonId : undefined}
        className="sr-only"
      />
      <span className="flex w-full flex-col items-center gap-[5px]">
        <span className={cn(!available && "opacity-60")}>
          <BarrierArt cc={cc} size={88} />
        </span>
        <span className="flex items-center gap-2">
          <strong id={nameId} className={cn("text-sm", tint.text)}>{meta.label}</strong>
          <BarrierStrengthStrip tier={cc} />
          <BarrierInfo cc={cc} />
        </span>
      </span>
      <strong className="text-sm text-foreground">{meta.short}</strong>
      <span className="flex min-h-5 items-center justify-center gap-1">
        {defaultBadge?.cc === cc && <Chip tone="info">{defaultBadge.text}</Chip>}
        {badge && <Chip>{badge}</Chip>}
      </span>
      <span
        aria-hidden
        className={cn(
          "grid size-[18px] place-items-center rounded-full border border-border-strong bg-card after:size-2 after:rounded-full after:content-['']",
          selected && tint.dot,
          !available && "bg-surface-2 opacity-45",
        )}
      />
      {!available && reason && (
        <span id={reasonId} className="flex w-full items-start justify-center gap-1.5 text-left text-sm leading-snug text-muted-foreground">
          <Lock className="mt-0.5 size-4 shrink-0" aria-hidden />
          <span>{reason}</span>
        </span>
      )}
    </label>
  );
}

// PLACEHOLDER for the shared InfoTip (E1a, info-tip.tsx): same contract (a
// labelled icon button that opens on click or tap and closes on Esc), so the
// merge swaps this for that import.
function BarrierInfo({ cc }: { cc: ConfinementClass }) {
  const meta = CC_META[cc];
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={TIER_PICKER.ABOUT(meta.label)}
          className={cn("flex size-4 shrink-0 items-center justify-center", TINT[cc].text)}
        >
          <Info className="size-4" aria-hidden />
        </button>
      </PopoverTrigger>
      <PopoverContent className="w-72 text-xs">
        <p className="text-sm font-medium text-foreground">{INFO_HEADINGS.protects}</p>
        <p className="mt-1 text-muted-foreground">{meta.protects}</p>
        <p className="mt-2 text-sm font-medium text-foreground">{INFO_HEADINGS.doesntProtect}</p>
        <p className="mt-1 text-muted-foreground">{meta.doesntProtect}</p>
        <p className="mt-2 text-sm font-medium text-foreground">{INFO_HEADINGS.howItWorks}</p>
        <p className="mt-1 text-muted-foreground">{meta.mechanism}</p>
      </PopoverContent>
    </Popover>
  );
}
