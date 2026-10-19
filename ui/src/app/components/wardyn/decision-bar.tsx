/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Clock } from "lucide-react";
import { Button } from "../ui/button";
import { RUN_HEADER } from "./copy/run-header";

/**
 * "N decisions need you" with a Review button, for a run whose pending requests the live strip cannot
 * carry. The title names the first request so the bar says what, not only how many.
 */
export function DecisionBar({ count, title, onReview }: { count: number; title?: string; onReview: () => void }) {
  return (
    <div
      data-testid="decision-bar"
      className="flex flex-col gap-2.5 rounded-xl border border-warning/40 bg-warning-subtle px-4 py-3 sm:flex-row sm:items-center sm:gap-3"
    >
      <Clock aria-hidden="true" className="hidden size-4 shrink-0 text-warning sm:block" />
      <p className="min-w-0 flex-1 text-sm">
        <strong className="font-semibold text-foreground">{RUN_HEADER.DECISIONS_NEED_YOU(count)}</strong>
        {title && <span className="text-muted-foreground"> · {title}</span>}
      </p>
      <Button size="sm" className="w-full sm:w-auto" onClick={onReview}>
        {RUN_HEADER.REVIEW}
      </Button>
    </div>
  );
}
