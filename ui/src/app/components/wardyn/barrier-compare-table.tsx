/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The barrier comparison control: ONE radiogroup whose radios sit in the column
// headers. At md+ it renders as a real <table> (column and row headers); below
// md as one self-labelled card per barrier. Both layouts are in the markup and
// CSS display shows exactly one, so the hidden set is out of the accessibility
// tree and the page never has two live radio sets.
import * as React from "react";
import { Info, Lock } from "lucide-react";
import { cn } from "../ui/utils";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { BarrierArt } from "./barrier-art";
import { BarrierMark } from "./barrier-mark";
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
  return (
    <div role="radiogroup" aria-label={label}>
      <div className="hidden md:block">
        <table className="w-full border-collapse text-sm">
          <caption className="sr-only">{TIER_PICKER.COMPARE_BARRIERS_TITLE}</caption>
          <thead>
            <tr>
              <th scope="col" className="w-56 px-3 pb-2 text-left align-bottom text-xs font-normal text-muted-foreground">
                {TIER_PICKER.COMPARE_BARRIERS_TITLE}
              </th>
              {options.map((o) => (
                <th key={o.cc} scope="col" className="px-1.5 pb-2 text-left align-top font-normal">
                  <BarrierChoice option={o} instance="table" artSize={88} {...shared} />
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {CC_MATRIX_ROWS.map((row) => (
              <tr key={row.label}>
                <th scope="row" className="border-t px-3 py-2 text-left align-middle font-normal text-foreground">
                  {row.label}
                </th>
                {options.map((o) => (
                  <td key={o.cc} className="border-t px-3 py-2 text-center align-middle">
                    <Mark mark={row.cells[o.cc]} cc={o.cc} />
                  </td>
                ))}
              </tr>
            ))}
            {showWhere && (
              <tr>
                <th scope="row" className="border-t px-3 py-2 text-left align-middle font-normal text-foreground">
                  {CC_MATRIX_WHERE.label}
                </th>
                {options.map((o) => (
                  <td key={o.cc} className="border-t px-3 py-2 text-xs text-muted-foreground">
                    {CC_MATRIX_WHERE.cells[o.cc]}
                  </td>
                ))}
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="flex flex-col gap-3 md:hidden">
        {options.map((o) => (
          <section key={o.cc} className="flex flex-col gap-3 rounded-lg border p-3">
            <BarrierChoice option={o} instance="card" artSize={56} {...shared} />
            <dl className="flex flex-col gap-2 text-sm">
              {CC_MATRIX_ROWS.map((row) => (
                <div key={row.label} className="flex items-center justify-between gap-3">
                  <dt className="text-foreground">{row.label}</dt>
                  <dd className="shrink-0">
                    <Mark mark={row.cells[o.cc]} cc={o.cc} />
                  </dd>
                </div>
              ))}
              {showWhere && (
                <div className="flex items-center justify-between gap-3">
                  <dt className="text-foreground">{CC_MATRIX_WHERE.label}</dt>
                  <dd className="text-right text-xs text-muted-foreground">{CC_MATRIX_WHERE.cells[o.cc]}</dd>
                </div>
              )}
            </dl>
          </section>
        ))}
      </div>
    </div>
  );
}

// A caveat names its residual risk as text a screen reader reads, not only as a
// hover title.
function Mark({ mark, cc }: { mark: Parameters<typeof MarkCell>[0]["mark"]; cc: ConfinementClass }) {
  return (
    <>
      <MarkCell mark={mark} cc={cc} caveatPrefix={CAVEAT_PREFIX} />
      {mark === "caveat" && (
        <span className="sr-only">
          {CAVEAT_PREFIX} {CC_META[cc].doesntProtect}
        </span>
      )}
    </>
  );
}

function BarrierChoice({
  option,
  instance,
  artSize,
  uid,
  value,
  onChange,
  defaultBadge,
  badges,
}: {
  option: BarrierOption;
  instance: "table" | "card";
  artSize: 56 | 88;
  uid: string;
  value?: ConfinementClass;
  onChange: (cc: ConfinementClass) => void;
  defaultBadge?: { cc: ConfinementClass; text: string };
  badges?: Partial<Record<ConfinementClass, string>>;
}) {
  const { cc, available, reason } = option;
  const meta = CC_META[cc];
  const nameId = `${uid}-${instance}-${cc}-name`;
  const reasonId = `${uid}-${instance}-${cc}-reason`;
  const badge = badges?.[cc];
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-start gap-1">
        <label
          className={cn(
            "flex flex-1 flex-col gap-2 rounded-lg border p-3",
            "has-[:checked]:border-primary has-[:checked]:bg-primary/10 has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring",
            available ? "cursor-pointer" : "cursor-not-allowed opacity-60",
          )}
        >
          <span className="flex items-start justify-between gap-2">
            <BarrierArt cc={cc} size={artSize} />
            <input
              type="radio"
              name={`${uid}-${instance}-barrier`}
              value={cc}
              checked={value === cc}
              disabled={!available}
              onChange={() => onChange(cc)}
              aria-labelledby={nameId}
              aria-describedby={!available && reason ? reasonId : undefined}
              className="mt-0.5 size-4 accent-primary"
            />
          </span>
          <BarrierMark cc={cc} strength nameId={nameId} />
          <span className="text-xs font-medium text-muted-foreground">{meta.short}</span>
          {(defaultBadge?.cc === cc || badge) && (
            <span className="flex flex-wrap gap-1">
              {defaultBadge?.cc === cc && <Chip>{defaultBadge.text}</Chip>}
              {badge && <Chip>{badge}</Chip>}
            </span>
          )}
        </label>
        <BarrierInfo cc={cc} />
      </div>
      {!available && reason && (
        <p id={reasonId} className="flex items-start gap-1.5 text-xs text-muted-foreground">
          <Lock className="mt-0.5 size-3 shrink-0" aria-hidden />
          <span>{reason}</span>
        </p>
      )}
    </div>
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
          className="mt-1 flex size-6 shrink-0 items-center justify-center rounded-full text-muted-foreground hover:text-foreground"
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
