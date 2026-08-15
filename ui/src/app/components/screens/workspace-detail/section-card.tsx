/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one card shell every workspace detail section uses — pulled into its
// own tiny file here since FOUR sibling cards need it (avoids either
// duplicating it four times or a circular import with the main screen
// file). Deliberately its own, larger style: run-detail.tsx actually
// imports wardyn/primitives.tsx's SectionCard (p-4, small-caps uppercase
// eyebrow title, no subtitle) — this one uses p-5, a bold sentence-case
// title, and a `subtitle` line every one of this screen's four sections
// relies on. Don't collapse the two without adding subtitle support there.
import type * as React from "react";

export function SectionCard({
  title,
  subtitle,
  right,
  children,
}: {
  title: string;
  subtitle?: string;
  right?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <section className="rounded-xl border border-border bg-card p-5">
      <div className="mb-3.5 flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1">
          <h2 className="text-sm font-semibold text-foreground">{title}</h2>
          {subtitle && <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">{subtitle}</p>}
        </div>
        {right}
      </div>
      <div className="space-y-4">{children}</div>
    </section>
  );
}
