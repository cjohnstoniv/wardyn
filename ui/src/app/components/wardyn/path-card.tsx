/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Chip } from "./primitives";
import { cn } from "../ui/utils";

/** The visible word on a card that is not available yet. */
export const COMING_SOON = "Coming soon";

// Make's .path-card (snap-2125Z index.css:93-105, :510-512): 270 px tall (220 px at <=760 px), 22 px padding,
// a 36 px icon tile in teal with 36 px below it (24 px at <=760 px), an 18 px title, a 13 px body held at 65 px,
// the foot or the actions pushed to the bottom. The featured border and the Fastest badge's colour are not
// carried over (owner, 10-11 ~23:15Z: teal is for selection and primary actions only).
const CARD = "flex min-h-[270px] flex-col rounded-xl border bg-card p-[22px] text-left max-[760px]:min-h-[220px]";

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
      <span
        aria-hidden="true"
        className={cn(
          "mb-9 flex size-9 items-center justify-center rounded-[9px] max-[760px]:mb-6",
          comingSoon ? "bg-muted text-muted-foreground" : "bg-primary/10 text-primary",
        )}
      >
        {icon}
      </span>
      <span className="flex flex-wrap items-center gap-2">
        <span id={titleId} className="text-lg font-normal tracking-[-0.01em] text-foreground">
          {title}
        </span>
        {comingSoon ? <Chip>{COMING_SOON}</Chip> : badge && <Chip>{badge}</Chip>}
      </span>
      <span className="mt-2.5 min-h-[65px] text-[13px] text-muted-foreground max-[760px]:min-h-0">{description}</span>
      {foot && <span className={cn("mt-auto text-xs", onSelect && !comingSoon ? "font-semibold text-primary" : "text-muted-foreground")}>{foot}</span>}
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
        <div className="mt-auto flex flex-wrap gap-2">{actions}</div>
      </section>
    );
  }
  return (
    <button type="button" onClick={onSelect} className={cn(CARD, "w-full transition-[border-color,transform] hover:-translate-y-[3px] hover:border-border-strong motion-reduce:transition-none motion-reduce:hover:translate-y-0 focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring outline-none")}>
      {body}
    </button>
  );
}
