/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run screen's presentational primitives — the three stateless wrappers
// its two columns are built out of. Split out of new-run-screen.tsx (which the
// permissioning lane pushed past the 1000-line file gate) purely by seam: these
// take props and render, they read none of the screen's state, and nothing else
// in the file depends on them beyond calling them.

import * as React from "react";
import { cn } from "../../ui/utils";

export function SectionCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="rounded-xl border border-border bg-surface-1">
      <div className="border-b border-border px-4 py-2.5">
        <h3 className="text-sm font-medium text-foreground">{title}</h3>
      </div>
      <div className="p-4">{children}</div>
    </section>
  );
}

export function Seg({
  options,
  value,
  onChange,
  label,
}: {
  options: { id: string; label: string; disabled?: boolean }[];
  value: string;
  onChange: (id: string) => void;
  label: string;
}) {
  return (
    <div role="radiogroup" aria-label={label} className="flex flex-wrap gap-2">
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          role="radio"
          aria-checked={value === o.id}
          disabled={o.disabled}
          onClick={() => onChange(o.id)}
          className={cn(
            "rounded-lg border px-3 py-1.5 text-[0.8125rem] font-medium transition-colors",
            value === o.id
              ? "border-primary bg-primary/10 text-primary"
              : "border-border text-foreground hover:border-border-strong",
            o.disabled && "cursor-not-allowed opacity-40 hover:border-border",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function RailSection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="border-b border-border pb-3 last:border-0">
      <p className="mb-1.5 text-[0.625rem] font-medium tracking-wide text-muted-foreground uppercase">{title}</p>
      {children}
    </div>
  );
}
