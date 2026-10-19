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
import type { AuditEvent, RunDetail } from "../../../../lib/types";
import { absoluteTime } from "../../../../lib/format";
import { CopyButton } from "../../../wardyn/copy-button";
import { RUN_PLACEMENT, RUN_RECORDING, runnerLabel } from "../../../wardyn/copy/runners";
import { usePrincipal } from "../../../wardyn/operator-context";
import { WidgetCard } from "../../../wardyn/primitives";
import { IDENTITY_POLICY_VIEW } from "../policy-tab-copy";
import { ComputerFolderRows } from "./identity-computer-folder";
import { SyncRows } from "./identity-sync";

export function IdentityWidget({
  run,
  audit = [],
  onGoPolicy,
  onGoAudit = () => {},
}: {
  run: RunDetail;
  /** The run's audit rows, for the Sync rows. */
  audit?: AuditEvent[];
  onGoPolicy: () => void;
  onGoAudit?: () => void;
}) {
  // UT-7a: "Ran as {type}" reads the name GET /runs/{id} resolved server-side
  // (RunDetail.user_type_name). Nothing renders for a run with no type or a
  // type since deleted — never the raw id.
  const ranAs = run.user_type_name;
  const principal = usePrincipal();
  // Where the run runs: local names the computer, remote says so. A run with no recorded placement (from
  // before 0.9, or on a deployment that places nothing) has no such row. The runner and pool rows name where
  // it went, by id when no name is known.
  const local = run.placement === "local";
  const runsOn = local
    ? principal && run.created_by === principal
      ? RUN_PLACEMENT.MINE(runnerLabel(run))
      : RUN_PLACEMENT.OTHERS(runnerLabel(run), run.created_by)
    : run.placement === "remote"
      ? RUN_PLACEMENT.REMOTE
      : null;
  const runner = run.runner_name || run.runner_id || run.remote_runner_id || run.runner_target;
  const pool = run.runner_pool_name || run.runner_pool_id;

  return (
    <WidgetCard title="Identity" Icon={Fingerprint} grow>
      {/* The command bar's h1 is the run's TITLE now, and it truncates to one
          line in the bar's 52px single row at xl and up (wraps below —
          review R-16) — so the task (the actual prompt the agent was given)
          and the description have no other home on the page. Prose, above
          the dl, and only when there is something to say. */}
      {(run.task || run.description) && (
        <div className="mb-3 space-y-2 border-b border-border pb-3">
          {run.task && (
            <div>
              <p className="mb-0.5 text-xs text-muted-foreground">Task</p>
              <p className="whitespace-pre-wrap text-xs leading-snug text-foreground">
                {run.task}
              </p>
            </div>
          )}
          {run.description && (
            <div>
              <p className="mb-0.5 text-xs text-muted-foreground">Why</p>
              <p className="whitespace-pre-wrap text-xs leading-snug text-muted-foreground">
                {run.description}
              </p>
            </div>
          )}
        </div>
      )}
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 text-xs">
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

        {/* Every run has a policy, so the row is unconditional; the value opens
            the Policy tab, and the saved policy's id stays on the hover title. */}
        <dt className="text-muted-foreground">Policy</dt>
        <dd className="min-w-0 truncate text-right">
          <button
            type="button"
            onClick={onGoPolicy}
            title={run.policy_id}
            className="font-medium text-primary hover:underline"
          >
            {IDENTITY_POLICY_VIEW}
          </button>
        </dd>

        {runsOn && (
          <>
            <dt className="text-muted-foreground">{RUN_PLACEMENT.LABEL}</dt>
            <dd className="min-w-0 text-right text-foreground">
              <div className="truncate">{runsOn}</div>
              {run.placement_filled && <div className="text-muted-foreground">{RUN_PLACEMENT.FILLED}</div>}
            </dd>
          </>
        )}

        <dt className="text-muted-foreground">{RUN_PLACEMENT.RUNNER}</dt>
        <dd className="min-w-0 truncate text-right font-mono text-foreground" title={run.runner_id ?? run.remote_runner_id}>
          {runner}
        </dd>

        {pool && (
          <>
            <dt className="text-muted-foreground">{RUN_PLACEMENT.POOL}</dt>
            <dd className="min-w-0 truncate text-right text-foreground" title={run.runner_pool_id}>
              {pool}
            </dd>
          </>
        )}

        {local && run.evidence_source === "runner_asserted" && (
          <>
            <dt className="text-muted-foreground">{RUN_PLACEMENT.EVIDENCE}</dt>
            <dd className="min-w-0 truncate text-right text-foreground">{RUN_PLACEMENT.REPORTED_BY(runnerLabel(run))}</dd>
          </>
        )}

        {run.not_recorded && (
          <>
            <dt className="text-muted-foreground">{RUN_RECORDING.LABEL}</dt>
            <dd className="min-w-0 text-right text-foreground">{RUN_RECORDING[run.not_recorded]}</dd>
          </>
        )}

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

        {ranAs && (
          <>
            <dt className="text-muted-foreground">Ran as</dt>
            <dd className="min-w-0 truncate text-right text-foreground">{ranAs}</dd>
          </>
        )}

        <ComputerFolderRows run={run} />
        <SyncRows run={run} audit={audit} onGoAudit={onGoAudit} />
      </dl>
      {local && run.evidence_source === "runner_asserted" && (
        <p className="mt-2 text-meta leading-snug text-muted-foreground">{RUN_PLACEMENT.REPORTED_NOTE(runnerLabel(run))}</p>
      )}
    </WidgetCard>
  );
}
