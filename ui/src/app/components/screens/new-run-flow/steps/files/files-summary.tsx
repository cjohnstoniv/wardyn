/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one-line summary under the step, announced politely and only once it
// has held still for half a second, so typing a size does not chatter.

import * as React from "react";
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
  /** Names of the included drives: the ones whose changes are kept. */
  keptDrives: readonly string[];
  life: Lifecycle;
}

export function summaryText(f: SummaryFacts): string {
  const life = lifeFragment(f.life);
  if (f.noExtra) return life ? NR_FILES.SUMMARY_NO_EXTRA(f.size, life) : NR_FILES.SUMMARY_MAIN(f.size);
  if (!f.chosen || !f.startsName) return NR_FILES.CHOOSE_FILES;
  const parts = [
    NR_FILES.SUMMARY_MAIN(f.size),
    NR_FILES.SUMMARY_STARTS(f.startsName),
    // One canon clause per drive, joined with the canon separator: no local conjunction.
    ...f.keptDrives.map((name) => NR_FILES.SUMMARY_KEPT_ONE(name)),
    life,
  ].filter((p): p is string => !!p);
  return parts.reduce((all, p) => NR_SHARED.JOIN(all, p));
}

export function FilesSummary({ text }: { text: string }) {
  const [shown, setShown] = React.useState(text);
  React.useEffect(() => {
    const timer = setTimeout(() => setShown(text), SUMMARY_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [text]);
  return <p role="status" aria-live="polite" className="text-sm font-medium">{shown}</p>;
}
