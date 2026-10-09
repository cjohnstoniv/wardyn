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
}) {
  return (
    <div className="inline-flex w-fit overflow-hidden rounded-lg border border-border-strong">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          disabled={disabled}
          onClick={() => onChange(o.value)}
          className={cn(
            "flex items-center gap-1.5 border-l border-border px-3 py-1.5 text-xs transition-colors first:border-l-0 disabled:cursor-not-allowed disabled:opacity-50",
            value === o.value
              ? o.value === "deny"
                ? "bg-danger-subtle font-medium text-danger"
                : "bg-muted font-medium text-foreground"
              : "text-muted-foreground hover:text-foreground",
          )}
        >
          {o.label}
          {/* aria-hidden: the chip is a VISUAL echo of a fact already
              announced elsewhere (the PageHeader chip, the beside-Save
              marker) — folding its text into this button's accessible name
              would silently break every exact-string `getByRole(...,
              {name: o.label})` lookup the moment the tab it names is dirty. */}
          {o.dirty && (
            <span aria-hidden="true" data-testid={`tab-dirty-chip-${o.value}`}>
              <Chip tone="warning">{UNSAVED.DIRTY_CHIP}</Chip>
            </span>
          )}
        </button>
      ))}
    </div>
  );
}
