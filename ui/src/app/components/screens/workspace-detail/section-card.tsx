/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one card shell every workspace detail section uses — same rounded-card
// idiom run-detail.tsx's own (local) SectionCard uses, pulled into its own
// tiny file here since FOUR sibling cards need it (avoids either duplicating
// it four times or a circular import with the main screen file).
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
