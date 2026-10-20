/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one ordered blocker list. The container always exists: the held Launch
// button is described by it, and it is the polite status region that says why.

import { CircleAlert } from "lucide-react";
import { Button } from "../../../ui/button";
import { cn } from "../../../ui/utils";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import type { NewRunLocation } from "../draft/new-run-url";
import type { Blocker } from "./blockers";

export const BLOCKERS_ID = "nr-blockers";

export function BlockerList({
  blockers,
  go,
  onCheckAgain,
}: {
  blockers: readonly Blocker[];
  go: (loc: NewRunLocation) => void;
  /** The stale result's fix sits in its own card (#3043); the footer's Check again stays too. */
  onCheckAgain?: () => void;
}) {
  // A stale result blames no group, so its card is amber; any blocker that names something stays red.
  const stale = blockers.length > 0 && blockers.every((b) => b.kind === "stale");
  return (
    <section
      id={BLOCKERS_ID}
      role="status"
      className={
        blockers.length
          ? cn("rounded-lg border p-3 text-sm", stale ? "border-warning/25 bg-warning-subtle" : "border-danger/25 bg-danger-subtle")
          : undefined
      }
    >
      {blockers.length > 0 && (
        <>
          <p className="flex items-center gap-2 font-medium text-foreground">
            <CircleAlert aria-hidden="true" className={cn("size-4 shrink-0", stale ? "text-warning" : "text-danger")} />
            {blockers.length === 1 ? NR_REVIEW.BLOCKERS_ONE : NR_REVIEW.BLOCKERS_MANY(blockers.length)}
          </p>
          <ul className="mt-2 flex flex-col gap-1 pl-6">
            {blockers.map((b) => (
              <li key={b.id}>
                {b.to ? (
                  <button type="button" className="text-left text-foreground underline underline-offset-2 hover:no-underline" onClick={() => b.to && go(b.to)}>
                    {b.text}
                  </button>
                ) : (
                  <span>{b.text}</span>
                )}
              </li>
            ))}
          </ul>
          {stale && onCheckAgain && (
            <Button type="button" variant="outline" size="sm" className="mt-3" onClick={onCheckAgain}>
              {NR_REVIEW.CHECK_AGAIN}
            </Button>
          )}
        </>
      )}
    </section>
  );
}
