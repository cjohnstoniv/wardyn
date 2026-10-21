/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { CircleHelp, Lock } from "lucide-react";
import { cn } from "../ui/utils";
import { Chip } from "./primitives";
import { REFUSAL } from "./copy/refusal";

export type RefusalRow = { label: string; value: string; muted?: boolean };

// The approved "Why can't I?" card (T8, R6RUN #3047), with exactly one action. `tone="blocked"` is a rule that
// refused the viewer; `tone="absent"` is a dashed card for something that is not there to be refused.
export function Refusal({
  title,
  chip,
  body,
  rows,
  tone = "blocked",
  ariaLabel = REFUSAL.ARIA,
  children,
}: {
  title: string;
  chip: string;
  body: string;
  rows: RefusalRow[];
  tone?: "blocked" | "absent";
  ariaLabel?: string;
  /** The one action. */
  children: React.ReactNode;
}) {
  const blocked = tone === "blocked";
  const Icon = blocked ? Lock : CircleHelp;
  return (
    <section
      aria-label={ariaLabel}
      data-testid="refusal"
      className={cn(
        "flex w-full max-w-xl flex-col gap-3.5 rounded-xl border px-5 py-4",
        blocked ? "border-danger/30 bg-danger-subtle" : "border-dashed border-border-strong bg-card",
      )}
    >
      <div className="flex items-center gap-2.5">
        <Icon className={cn("size-[18px] shrink-0", blocked ? "text-danger" : "text-muted-foreground")} aria-hidden />
        <h1 className="flex-1 text-sm font-semibold text-foreground">{title}</h1>
        <Chip tone={blocked ? "danger" : "neutral"}>{chip}</Chip>
      </div>
      <p className="text-sm text-foreground">{body}</p>
      <dl className="grid grid-cols-[6rem_minmax(0,1fr)] gap-x-3.5 gap-y-2.5 text-sm">
        {rows.map((r) => (
          <React.Fragment key={r.label}>
            <dt className="text-muted-foreground">{r.label}</dt>
            <dd className={cn(r.muted && "text-muted-foreground")}>{r.value}</dd>
          </React.Fragment>
        ))}
      </dl>
      <div>{children}</div>
    </section>
  );
}
