/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Identity widget — the rail's last widget (`grow`, absorbs leftover column
// height). A small dl of the run's addressable identity. Repo + workspace
// path moved to the command bar in this redesign, so they are DELIBERATELY
// not repeated here.
import { Fingerprint } from "lucide-react";
import type { AgentRun } from "../../../../lib/types";
import { absoluteTime } from "../../../../lib/format";
import { CopyButton } from "../../../wardyn/copy-button";
import { WidgetCard } from "../../../wardyn/primitives";

export function IdentityWidget({ run }: { run: AgentRun }) {
  return (
    <WidgetCard title="Identity" Icon={Fingerprint} grow>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 text-[0.75rem]">
        <dt className="text-muted-foreground">Run</dt>
        <dd className="min-w-0 truncate text-right font-mono text-foreground" title={run.id}>
          {run.id}
        </dd>

        <dt className="text-muted-foreground">Identity</dt>
        <dd className="flex min-w-0 items-center justify-end gap-1.5">
          <span className="min-w-0 truncate font-mono text-foreground" title={run.spiffe_id}>
            {run.spiffe_id}
          </span>
          <CopyButton
            text={run.spiffe_id}
            label="Copy identity"
            iconClassName="size-3"
            className="shrink-0 text-muted-foreground hover:text-foreground"
          />
        </dd>

        {run.image && (
          <>
            <dt className="text-muted-foreground">Image</dt>
            <dd className="min-w-0 truncate text-right font-mono text-foreground" title={run.image}>
              {run.image}
            </dd>
          </>
        )}

        <dt className="text-muted-foreground">Started</dt>
        <dd className="min-w-0 truncate text-right text-foreground">
          {absoluteTime(run.created_at)} · {run.created_by}
        </dd>
      </dl>
    </WidgetCard>
  );
}
