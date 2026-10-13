/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one-line summary under the step, sticky at the bottom of the window,
// announced politely and only once it has held still for half a second, so
// typing a size does not chatter. While nothing is chosen it is a prompt, and
// after a held Continue an error, with the alert icon, never a check.

import * as React from "react";
import { TriangleAlert, Check } from "lucide-react";
import { cn } from "../../../../ui/utils";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { Lifecycle } from "./main-drive-section";

export const SUMMARY_DEBOUNCE_MS = 500;

const lifeFragment = (l: Lifecycle): string | undefined =>
  l.kind === "deleted" ? NR_FILES.SUMMARY_DELETED : l.kind === "noEnd" ? NR_FILES.SUMMARY_NO_END : l.kind === "kept" ? NR_FILES.SUMMARY_KEPT(l.grace) : undefined;

export interface SummaryFacts {
  /** The main drive's size in GiB, as shown. */
  size: string;
  noExtra: boolean;
  /** Something is included. */
  chosen: boolean;
  startsName?: string;
  /** Names of the included drives and folders: the ones whose changes are kept. */
  keptDrives: readonly string[];
  life: Lifecycle;
}

/** "A", "A and B", "A, B and C", from the canon's two joiners. */
export function listNames(names: readonly string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return NR_FILES.LIST_AND(names.slice(0, -1).reduce((all, n) => NR_FILES.LIST_COMMA(all, n)), names[names.length - 1]);
}

export function summaryText(f: SummaryFacts): string {
  const life = lifeFragment(f.life);
  if (f.noExtra) return life ? NR_FILES.SUMMARY_NO_EXTRA(f.size, life) : NR_FILES.SUMMARY_MAIN(f.size);
  if (!f.chosen || !f.startsName) return NR_FILES.CHOOSE_FILES;
  const parts = [
    NR_FILES.SUMMARY_MAIN(f.size),
    NR_FILES.SUMMARY_STARTS(f.startsName),
    f.keptDrives.length === 1 ? NR_FILES.SUMMARY_KEPT_ONE(f.keptDrives[0]) : f.keptDrives.length > 1 ? NR_FILES.SUMMARY_KEPT_MANY(listNames(f.keptDrives)) : undefined,
    life,
  ].filter((p): p is string => !!p);
  return parts.reduce((all, p) => NR_SHARED.JOIN(all, p));
}

export type SummaryTone = "done" | "prompt" | "error";

export function FilesSummary({ text, tone = "done" }: { text: string; tone?: SummaryTone }) {
  // The sentence and its icon change together, so a check never sits beside the empty-state sentence.
  const [shown, setShown] = React.useState({ text, tone });
  React.useEffect(() => {
    const timer = setTimeout(() => setShown({ text, tone }), SUMMARY_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [text, tone]);
  return (
    <div
      data-testid="nr-files-summary"
      data-tone={shown.tone}
      className={cn(
        "sticky bottom-2.5 z-[8] flex items-start gap-2 rounded-[9px] border bg-card/95 px-3.5 py-3 shadow-floating",
        shown.tone === "error" ? "border-[color-mix(in_srgb,var(--danger)_40%,var(--border))]" : "border-[color-mix(in_srgb,var(--primary)_35%,var(--border))]",
      )}
    >
      <span aria-hidden="true" className={cn("mt-0.5", shown.tone === "done" ? "text-primary" : shown.tone === "error" ? "text-danger" : "text-muted-foreground")}>
        {shown.tone === "done" ? <Check className="size-[15px]" /> : <TriangleAlert className="size-[15px]" />}
      </span>
      <p role="status" aria-live="polite" className="text-sm leading-[1.45] font-semibold">{shown.text}</p>
    </div>
  );
}
