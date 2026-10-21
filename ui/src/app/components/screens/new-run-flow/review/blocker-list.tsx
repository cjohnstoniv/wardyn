/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one ordered blocker list. The container always exists: the held Launch
// button is described by it, and it is the polite status region that says why.

import { CircleAlert } from "lucide-react";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import type { NewRunLocation } from "../draft/new-run-url";
import type { Blocker } from "./blockers";

export const BLOCKERS_ID = "nr-blockers";

export function BlockerList({ blockers, go }: { blockers: readonly Blocker[]; go: (loc: NewRunLocation) => void }) {
  return (
    <section id={BLOCKERS_ID} role="status" className={blockers.length ? "rounded-lg border border-danger/25 bg-danger-subtle p-3 text-sm" : undefined}>
      {blockers.length > 0 && (
        <>
          <p className="flex items-center gap-2 font-medium text-foreground">
            <CircleAlert aria-hidden="true" className="size-4 shrink-0 text-danger" />
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
                {b.hint && <p className="text-xs text-muted-foreground">{b.hint}</p>}
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  );
}
