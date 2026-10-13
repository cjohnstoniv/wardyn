/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ReactNode } from "react";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";

/**
 * One question of the Run step in its own bordered block, the question at the left and "Required" at the right
 * (Make App.tsx:1486-1490, 1690-1694; snapshot index.css:130-133). "Required" is neutral (DECISIONS 10-11 ~23:15Z).
 */
export function QuestionBlock({ id, labelId, label, children }: { id?: string; labelId?: string; label: string; children: ReactNode }) {
  return (
    <section id={id} className="rounded-[11px] border border-border bg-card p-5 max-[520px]:p-3.5">
      <div className="mb-3 flex items-center justify-between gap-3 text-xs font-semibold">
        <span id={labelId}>{label}</span>
        <span className="text-[10px] font-normal tracking-[0.08em] text-muted-foreground uppercase">{NR_SHARED.REQUIRED}</span>
      </div>
      {children}
    </section>
  );
}
