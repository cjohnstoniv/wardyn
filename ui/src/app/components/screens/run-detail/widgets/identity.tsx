/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Identity widget — the rail's last widget (`grow`, absorbs leftover column
// height). A small dl of the run's addressable identity. Repo + workspace
// path moved to the command bar in this redesign, so they are DELIBERATELY
// not repeated here.
//
// Policy / Runner / Sandbox are NOT decoration and are not optional to keep:
// they were on the card this widget replaced, and a review caught that the
// port silently dropped all three. On a governance console the run's governing
// POLICY disappearing from the run's own identity surface is the serious one —
// it is the answer to "what was this agent actually allowed to do", and the
// rest of the page never states it.
import { Fingerprint } from "lucide-react";
import type { AgentRun } from "../../../../lib/types";
import { absoluteTime } from "../../../../lib/format";
import { CopyButton } from "../../../wardyn/copy-button";
import { WidgetCard } from "../../../wardyn/primitives";

export function IdentityWidget({ run }: { run: AgentRun }) {
  return (
    <WidgetCard title="Identity" Icon={Fingerprint} grow>
      {/* The command bar's h1 is the run's TITLE now, and it truncates to one
          line in a 52px non-wrapping row — so the task (the actual prompt the
          agent was given) and the description have no other home on the page.
          Prose, above the dl, and only when there is something to say. */}
      {(run.task || run.description) && (
        <div className="mb-3 space-y-2 border-b border-border pb-3">
          {run.task && (
            <div>
              <p className="mb-0.5 text-[0.75rem] text-muted-foreground">Task</p>
              <p className="whitespace-pre-wrap text-[0.75rem] leading-snug text-foreground">
                {run.task}
              </p>
            </div>
          )}
          {run.description && (
            <div>
              <p className="mb-0.5 text-[0.75rem] text-muted-foreground">Why</p>
              <p className="whitespace-pre-wrap text-[0.75rem] leading-snug text-muted-foreground">
                {run.description}
              </p>
            </div>
          )}
        </div>
      )}
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

        {run.policy_id && (
          <>
            <dt className="text-muted-foreground">Policy</dt>
            <dd className="min-w-0 truncate text-right font-mono text-foreground" title={run.policy_id}>
              {run.policy_id}
            </dd>
          </>
        )}

        <dt className="text-muted-foreground">Runner</dt>
        <dd className="min-w-0 truncate text-right font-mono text-foreground">{run.runner_target}</dd>

        {run.sandbox_ref && (
          <>
            <dt className="text-muted-foreground">Sandbox</dt>
            <dd className="min-w-0 truncate text-right font-mono text-foreground" title={run.sandbox_ref}>
              {run.sandbox_ref}
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
