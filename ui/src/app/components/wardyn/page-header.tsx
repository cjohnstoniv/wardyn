/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

export function PageHeader({
  title,
  titleBadge,
  description,
  actions,
  as: Heading = "h1",
}: {
  title: string;
  /** #460 review — a small status marker (the dirty chip) that belongs BESIDE
   *  the title text itself, not off in the far-right `actions` slot (which
   *  `justify-between`s away from it on any page wide enough to have both). */
  titleBadge?: React.ReactNode;
  // ReactNode, not string: a frozen page lead may carry a mount target or an
  // env var that renders mono, and the mono span is applied at the CALL SITE
  // (the withMono precedent EmptyState.description already follows) rather than
  // baked into the copy. Plain strings still pass unchanged.
  description?: React.ReactNode;
  actions?: React.ReactNode;
  /** Heading level. Defaults to h1 — the page's own title. A pane rendered
   *  INSIDE another page (SshKeysPane on /settings) passes "h3" so the document
   *  keeps exactly one h1 and the outline stays honest. */
  as?: "h1" | "h3";
}) {
  const size = Heading === "h1" ? "text-[1.75rem] font-bold" : "text-sm font-medium";
  // #2831 (owner option B): below sm the title and the action share one row, the action pinned right and the
  // title wrapping beside it, and the description drops under both at full width. The wrapper goes `contents`
  // so the title row, the description and the actions can be ordered as one flex-wrap row; from sm up it is the
  // block it always was, so the desktop layout and DOM order do not move.
  return (
    <div className="mb-5 flex flex-wrap items-start justify-between gap-4 sm:flex-nowrap">
      <div className="contents sm:block sm:space-y-1">
        <div className="flex min-w-0 flex-1 items-center gap-2">
          <Heading className={`${size} leading-tight text-foreground`}>{title}</Heading>
          {titleBadge}
        </div>
        {description && (
          <p data-testid="page-header-description" className="order-last basis-full text-sm text-muted-foreground">
            {description}
          </p>
        )}
      </div>
      {actions && (
        <div data-testid="page-header-actions" className="flex shrink-0 items-center gap-2">
          {actions}
        </div>
      )}
    </div>
  );
}
