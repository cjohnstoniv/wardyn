/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's clone note (#3159): each repository that was skipped, failed or went unreported. Self-contained, as
// LaunchWarningsNote is: it reads the run id from the route and calls GET /runs/{id}/clones itself, so run-detail.tsx
// needs only the mount. Its role="status" container is rendered from the first render and stays empty until there is
// something to say, so a note that fills it after a pending answer is announced. It polls every 10 s only while the
// answer is pending, and stops at the first one that is not.
import * as React from "react";
import { useParams } from "react-router-dom";
import { getRunClones, type RunClones, type RunRepoClone } from "../../../lib/api/run-clones";
import { Chip } from "../../wardyn/primitives";
import { CLONE_CAUSE_SENTENCE, CLONE_NOTE } from "./clone-status-note-copy";

export const CLONE_POLL_MS = 10_000;

const SHOWN = ["skipped", "failed", "not_reported"];

function sentence(r: RunRepoClone): string {
  if (r.outcome === "not_reported") return CLONE_NOTE.notReportedRow;
  const known = r.cause ? CLONE_CAUSE_SENTENCE[r.cause] : undefined;
  if (known) return known;
  return r.outcome === "skipped" ? CLONE_NOTE.skippedBare : CLONE_NOTE.failedBare;
}

function RowChip({ outcome }: { outcome: string }) {
  if (outcome === "skipped") return <Chip tone="warning">{CLONE_NOTE.chipSkipped}</Chip>;
  if (outcome === "failed") return <Chip tone="danger">{CLONE_NOTE.chipFailed}</Chip>;
  return <Chip tone="neutral">{CLONE_NOTE.notReported}</Chip>;
}

export function CloneStatusNote() {
  const { id = "" } = useParams();
  const [clones, setClones] = React.useState<RunClones | null>(null);
  const pending = clones?.state === "pending";
  React.useEffect(() => {
    let live = true;
    const read = () => {
      getRunClones(id).then(
        (c) => {
          if (live) setClones(c);
        },
        () => {
          // A hint: a failed read draws nothing, and the next poll (if pending) tries again.
        },
      );
    };
    if (clones === null) {
      read();
      return () => {
        live = false;
      };
    }
    if (!pending) return undefined;
    const t = window.setInterval(read, CLONE_POLL_MS);
    return () => {
      live = false;
      window.clearInterval(t);
    };
    // The first read runs once per run id; polling re-arms only on the pending flag.
    // eslint-disable-next-line react-hooks/exhaustive-deps -- clones is read only for its null-ness and pending flag
  }, [id, clones === null, pending]);

  const rows = (clones?.state === "reported" || clones?.state === "not_reported" ? clones.repos : []).filter((r) =>
    SHOWN.includes(r.outcome),
  );
  const onlyNotReported = rows.length > 0 && rows.every((r) => r.outcome === "not_reported");
  const hasSandboxRows = rows.some((r) => r.outcome !== "not_reported");
  return (
    <div role="status" data-testid="clone-status-note">
      {rows.length > 0 && (
        <div className="mx-4 mt-2 shrink-0 rounded-md border border-warning/30 bg-warning-subtle px-2.5 py-2 text-xs">
          <p className="font-medium text-foreground">
            {onlyNotReported ? CLONE_NOTE.titleNotReported : CLONE_NOTE.titleSomeNotCloned}
          </p>
          <ul className="mt-1 space-y-1.5">
            {rows.map((r) => (
              <li key={r.dest} className="min-w-0">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
                  {r.repo && <span className="break-all font-mono text-foreground">{r.repo}</span>}
                  <span className="break-all font-mono text-muted-foreground">{r.dest}</span>
                  <RowChip outcome={r.outcome} />
                </div>
                <p className="mt-0.5 text-muted-foreground">{sentence(r)}</p>
              </li>
            ))}
          </ul>
          {hasSandboxRows && <p className="mt-1.5 text-muted-foreground">{CLONE_NOTE.footer}</p>}
        </div>
      )}
    </div>
  );
}
