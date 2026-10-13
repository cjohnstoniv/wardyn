/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { ChevronRight } from "lucide-react";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
import { cn } from "../ui/utils";

type Layout = "card" | "row";
const LayoutContext = React.createContext<Layout>("card");

/**
 * One radiogroup for RadioCard (layout "card") and ChoiceRow (layout "row", the workload
 * chooser). Arrow keys rove; Radix owns selection and focus.
 */
export function RadioCardGroup({
  value,
  onValueChange,
  name,
  "aria-labelledby": labelledBy,
  layout = "card",
  className,
  children,
}: {
  value: string;
  onValueChange: (value: string) => void;
  name: string;
  "aria-labelledby": string;
  layout?: Layout;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <LayoutContext.Provider value={layout}>
      <RadioGroup
        name={name}
        value={value}
        onValueChange={onValueChange}
        aria-labelledby={labelledBy}
        className={cn(layout === "row" ? "gap-2" : "gap-3", className)}
      >
        {children}
      </RadioGroup>
    </LayoutContext.Provider>
  );
}

/**
 * An unavailable card is disabled, hatched, and says why in visible text that the radio
 * also carries as its description (never title alone: CONSOLE-RULES §8).
 */
export function RadioCard({
  value,
  title,
  description,
  meta,
  badge,
  unavailable,
}: {
  value: string;
  title: string;
  description?: string;
  meta?: React.ReactNode;
  badge?: React.ReactNode;
  unavailable?: { reason: string };
}) {
  const layout = React.useContext(LayoutContext);
  const uid = React.useId();
  const ids = { title: `${uid}-title`, desc: `${uid}-desc`, meta: `${uid}-meta`, badge: `${uid}-badge`, reason: `${uid}-reason` };
  const describedBy = [
    description && ids.desc,
    meta && ids.meta,
    badge && ids.badge,
    unavailable && ids.reason,
  ]
    .filter(Boolean)
    .join(" ");
  const quiet = unavailable ? "text-muted-foreground" : "text-foreground";
  return (
    <label
      className={cn(
        "flex gap-3 rounded-lg border bg-card p-3 text-left transition-colors",
        "has-[[data-state=checked]]:border-primary has-[[data-state=checked]]:bg-primary/10",
        "has-[button:focus-visible]:border-ring has-[button:focus-visible]:ring-[3px] has-[button:focus-visible]:ring-ring",
        unavailable ? "hatch-unavailable cursor-not-allowed" : "cursor-pointer hover:bg-accent has-[[data-state=checked]]:hover:bg-primary/10",
      )}
    >
      <RadioGroupItem
        value={value}
        disabled={!!unavailable}
        aria-labelledby={ids.title}
        aria-describedby={describedBy || undefined}
        className="mt-0.5 focus-visible:ring-0 disabled:opacity-100"
      />
      <span className="flex min-w-0 flex-1 flex-col gap-1">
        <span className="flex flex-wrap items-center gap-2">
          <span id={ids.title} className={cn("text-sm font-medium", quiet)}>
            {title}
          </span>
          {badge && <span id={ids.badge}>{badge}</span>}
        </span>
        {description && (
          <span id={ids.desc} className="text-xs leading-snug text-muted-foreground">
            {description}
          </span>
        )}
        {meta && (
          <span id={ids.meta} className="text-xs text-muted-foreground">
            {meta}
          </span>
        )}
        {unavailable && (
          <span id={ids.reason} className="text-xs text-muted-foreground">
            {unavailable.reason}
          </span>
        )}
      </span>
      {layout === "row" && <ChevronRight aria-hidden="true" className="mt-0.5 size-4 shrink-0 self-center text-muted-foreground" />}
    </label>
  );
}

/** The row layout under its design-system name; one implementation. */
export const ChoiceRow = RadioCard;
