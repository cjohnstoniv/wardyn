/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Chip } from "./primitives";
import { cn } from "../ui/utils";

/** The visible word on a card that is not available yet. */
export const COMING_SOON = "Coming soon";

const CARD = "flex flex-col gap-2 rounded-xl border bg-card p-4 text-left";

/**
 * A way in. Plain `onSelect` makes the whole card one button; `actions` makes it a region
 * holding its own buttons (never a button around buttons); `comingSoon` is aria-disabled
 * and says so in words.
 */
export function PathCard({
  title,
  description,
  badge,
  icon,
  foot,
  comingSoon,
  onSelect,
  actions,
}: {
  title: string;
  description: string;
  badge?: string;
  icon: React.ReactNode;
  foot?: React.ReactNode;
  comingSoon?: boolean;
  onSelect?: () => void;
  actions?: React.ReactNode;
}) {
  const titleId = React.useId();
  const body = (
    <>
      <span aria-hidden="true" className="flex size-9 items-center justify-center rounded-xl bg-muted text-muted-foreground">
        {icon}
      </span>
      <span className="flex flex-wrap items-center gap-2">
        <span id={titleId} className="text-base font-semibold text-foreground">
          {title}
        </span>
        {comingSoon ? <Chip>{COMING_SOON}</Chip> : badge && <Chip>{badge}</Chip>}
      </span>
      <span className="text-sm text-muted-foreground">{description}</span>
      {foot && <span className="mt-auto text-sm font-medium text-foreground">{foot}</span>}
    </>
  );
  if (comingSoon) {
    return (
      <article aria-disabled="true" aria-labelledby={titleId} className={cn(CARD, "hatch-unavailable")}>
        {body}
      </article>
    );
  }
  if (actions) {
    return (
      <section aria-labelledby={titleId} className={CARD}>
        {body}
        <div className="flex flex-wrap gap-2">{actions}</div>
      </section>
    );
  }
  return (
    <button type="button" onClick={onSelect} className={cn(CARD, "w-full transition-colors hover:bg-accent focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring outline-none")}>
      {body}
    </button>
  );
}
