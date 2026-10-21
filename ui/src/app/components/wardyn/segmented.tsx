/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { UNSAVED } from "../../lib/unsaved-copy";
import { cn } from "../ui/utils";
import { Chip } from "./primitives";

// A two- or three-way segmented picker. Buttons with aria-pressed, not tabs and
// not a Select: these are form choices, they are all visible at once in the
// mock, and a plain button is the one control that stays clickable in both the
// vitest and Playwright harnesses without a pointer-events dance.
// Shared by the lazy screens that offer such a choice (permissions, providers,
// governance, drives, the People step, the policy document's view switch). Its
// own module so none of them imports another screen for it, and so it never
// rides the eager shell chunk.
export function Segmented<T extends string>({
  value,
  options,
  onChange,
  disabled,
  size = "default",
}: {
  value: T;
  options: {
    value: T;
    label: string;
    /** #460 — this option's own draft differs from what loaded; renders the
     *  dirty chip beside its label, the tab's own "title". Optional: only
     *  providers-screen.tsx's Git/Storage/Agents options set it today. */
    dirty?: boolean;
  }[];
  onChange: (v: T) => void;
  disabled?: boolean;
  /** New Run's view and format switches: a padded track with an inset pill per option (the Make design's compact control). */
  size?: "default" | "compact";
}) {
  const chip = (o: { value: T; dirty?: boolean }) =>
    o.dirty && (
      /* aria-hidden: the chip is a VISUAL echo of a fact already
          announced elsewhere (the PageHeader chip, the beside-Save
          marker) — folding its text into this button's accessible name
          would silently break every exact-string `getByRole(...,
          {name: o.label})` lookup the moment the tab it names is dirty. */
      <span aria-hidden="true" data-testid={`tab-dirty-chip-${o.value}`}>
        <Chip tone="warning">{UNSAVED.DIRTY_CHIP}</Chip>
      </span>
    );
  if (size === "compact") {
    return (
      <div className="inline-flex w-fit rounded-[9px] border border-border bg-surface-2 p-[3px]">
        {options.map((o) => (
          <button
            key={o.value}
            type="button"
            aria-pressed={value === o.value}
            disabled={disabled}
            onClick={() => onChange(o.value)}
            className={cn(
              "flex min-w-[65px] items-center justify-center gap-1.5 rounded-md px-3 py-[7px] text-xs transition-colors disabled:cursor-not-allowed disabled:opacity-50",
              value === o.value ? (o.value === "deny" ? "bg-danger-subtle text-danger shadow-sm" : "bg-card text-foreground shadow-sm") : "text-muted-foreground hover:text-foreground",
            )}
          >
            {o.label}
            {chip(o)}
          </button>
        ))}
      </div>
    );
  }
  // The selected fill is the primary token: bg-muted sat at ~1.1:1 on the card, so only the bolder label told the options apart (#2832).
  // max-w-full + overflow-x-auto: an over-wide set scrolls inside its card instead of being clipped (#2870).
  return (
    <div className="inline-flex w-fit max-w-full overflow-x-auto rounded-lg border border-border-strong">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          disabled={disabled}
          onClick={() => onChange(o.value)}
          className={cn(
            "flex items-center gap-1.5 whitespace-nowrap border-l border-border px-3 py-1.5 text-xs transition-colors first:border-l-0 disabled:cursor-not-allowed disabled:opacity-50",
            value === o.value
              ? o.value === "deny"
                ? "bg-danger-subtle font-medium text-danger"
                : "bg-primary font-medium text-primary-foreground"
              : "text-muted-foreground hover:text-foreground",
          )}
        >
          {o.label}
          {chip(o)}
        </button>
      ))}
    </div>
  );
}
