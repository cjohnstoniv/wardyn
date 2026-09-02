/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

export function PageHeader({
  title,
  description,
  actions,
  as: Heading = "h1",
}: {
  title: string;
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
  return (
    <div className="mb-5 flex items-start justify-between gap-4">
      <div className="space-y-1">
        <Heading className={`${size} leading-tight text-foreground`}>{title}</Heading>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
    </div>
  );
}
