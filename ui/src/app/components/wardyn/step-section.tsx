/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { cn } from "../ui/utils";

// #3044 (R6NEW, approved 2026-10-20): the one section card of the guided steps and Review. A sentence-case title
// (which may hold a select), an optional description, a marker slot (Required, Optional or a status mark; never a
// pill, never caps), one action slot, and a body under a rule. Shape, Run, Harnesses and Files draw their
// sections with it, so one idea has one radius, one heading style and one marker style. The Tools step and
// Review's groups still draw their own cards; moving them over is a follow-up.
export function StepSection({
  title,
  titleId,
  titleAs: Title = "h2",
  description,
  marker,
  action,
  ariaLabel,
  className,
  bodyClassName,
  children,
}: {
  title: React.ReactNode;
  /** The id of the title element, for the section's (or a group's) aria-labelledby. */
  titleId?: string;
  /** The title's element; "div" where the section never had a heading. */
  titleAs?: "h2" | "h3" | "div";
  description?: React.ReactNode;
  /** Required, Optional or a status mark. */
  marker?: React.ReactNode;
  /** One button. */
  action?: React.ReactNode;
  /** Names the section when its title is not plain text (a select). */
  ariaLabel?: string;
  className?: string;
  bodyClassName?: string;
  children?: React.ReactNode;
}) {
  const hasBody = React.Children.toArray(children).length > 0;
  return (
    <section aria-label={ariaLabel} aria-labelledby={ariaLabel ? undefined : titleId} className={cn("rounded-xl border border-border bg-card", className)}>
      <div className={cn("flex items-start justify-between gap-3 px-4 py-3.5 max-[520px]:flex-wrap", hasBody && "border-b border-border")}>
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <Title id={titleId} className="text-[15px] leading-snug font-semibold text-foreground">
            {title}
          </Title>
          {description && <p className="text-body text-muted-foreground">{description}</p>}
        </div>
        {marker && <span className="shrink-0 text-xs text-muted-foreground">{marker}</span>}
        {action && <div className="shrink-0">{action}</div>}
      </div>
      {hasBody && <div className={cn("px-4 py-3.5", bodyClassName)}>{children}</div>}
    </section>
  );
}
