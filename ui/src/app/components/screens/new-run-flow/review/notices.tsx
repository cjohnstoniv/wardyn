/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The true notices: the pool is at its limit, a size was narrowed, and each of the server's own preflight warnings.

import { Notice } from "../../../wardyn/notice";
import { PolicyRemedy, safeRequestHref } from "../../../wardyn/policy-remedy";
import type { PolicyRef } from "../../../../lib/api/health";
import type { WarningContext } from "../../../../lib/types/preflight";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { RUN_WAIT } from "../../../../lib/runner-pool-refusals";
import type { AppliedAmount, AppliedResources } from "../../../../lib/types/new-run-contract";
import type { LimitSource } from "../../../../lib/types/runner-pools";
import { reviewWarningText, wallsHostKnob } from "./drop-warnings";
import { amountText } from "./format";
import type { PoolSlotsFact } from "./review-seams";

/**
 * composer.WarnResourcesCapped. A CPU or memory cut is said again, with its source, by the narrowed notice; a pids or
 * disk cut is not, and the server folds all of them into this one sentence. It is dropped only when the narrowed
 * notices are everything it can be saying.
 */
const RESOURCES_CAPPED = "resources capped to operator maximum";

/** What the person asked for; a zero asked for the default, which is then what they got. */
const asked = (a: AppliedAmount) => (a.requested > 0 ? a.requested : a.applied);

interface Narrowing {
  key: string;
  /** Who narrowed the request; absent when the source has no name of its own. */
  by?: string;
  tail: string;
}

function narrowing(amount: AppliedAmount, poolName: string | undefined): Narrowing | undefined {
  if (!amount.capped || amount.requested <= 0 || !amount.source) return undefined;
  const source: LimitSource = amount.source;
  const key = `${source}:${amount.source_name ?? ""}`;
  switch (source) {
    case "pool": {
      const pool = amount.source_name || poolName;
      return { key, by: pool, tail: NR_REVIEW.NARROWED_TAIL_POOL(pool ?? "") };
    }
    case "governance": return { key, by: amount.source_name, tail: NR_REVIEW.NARROWED_TAIL_CEILING };
    case "deployment": return { key, tail: NR_REVIEW.NARROWED_TAIL_SERVER };
    case "runner": return { key, tail: NR_REVIEW.NARROWED_TAIL_RUNNERS };
  }
}

export interface NoticesProps {
  poolName?: string;
  slots?: PoolSlotsFact;
  applied?: AppliedResources;
  /** The person set the size themselves, so the struck-through ask is theirs. */
  askedByYou: boolean;
  /** The preflight's `warnings`, the server's sentences, shown as they came. */
  warnings?: readonly string[];
  /** The preflight's `warning_context`, keyed to `warnings` by index. */
  warningContext?: readonly WarningContext[];
  /** The main drive was cut by a limit: that cut is folded into the shared "resources capped" warning too, as is a PIDs cut (applied.pids_limit). */
  diskCut?: boolean;
  /** The walls' host lists the Access group draws a "Your walls leave out {host}." line for; that line says the list's warning by host. */
  wallsLeftOut?: readonly string[];
}

/** The policy a drop warning names, only when it publishes a way to ask; otherwise the warning reads as it always did. */
function askablePolicy(ctx: WarningContext | undefined): PolicyRef | undefined {
  const p = ctx?.kind === "egress_policy_drop" ? ctx.policy : undefined;
  return p && (safeRequestHref(p.request_url) || p.email || p.request_text) ? p : undefined;
}

export function Notices({ poolName, slots, applied, askedByYou, warnings, warningContext, diskCut, wallsLeftOut }: NoticesProps) {
  const full = slots?.verdict === "waits" && slots.max !== undefined && !!poolName;
  const found = applied
    ? [narrowing(applied.cpu_millis, poolName), narrowing(applied.memory_mib, poolName)].filter((n): n is Narrowing => n !== undefined)
    : [];
  const distinct = found.filter((n, i) => found.findIndex((m) => m.key === n.key) === i);
  // Each warning keeps its index into the server's list, which is what its context names.
  const server = (warnings ?? [])
    .map((w, index) => ({ w, policy: askablePolicy(warningContext?.find((c) => c.warning_index === index)) }))
    .filter(({ w }) => w !== RESOURCES_CAPPED || (distinct.length === 0 || diskCut || !!applied?.pids_limit?.capped))
    .filter(({ w }) => { const knob = wallsHostKnob(w); return !(knob && wallsLeftOut?.includes(knob)); });
  if (!full && distinct.length === 0 && server.length === 0) return null;
  return (
    <div className="flex flex-col gap-3">
      {full && slots && poolName && (
        <Notice tone="info" live>
          {RUN_WAIT.AT_CAPACITY(poolName, slots.max ?? 0)}
        </Notice>
      )}
      {distinct.map((n) => applied && (
        <Notice key={n.key} tone="warning" title={n.by ? NR_REVIEW.NARROWED_BY(n.by) : undefined}>
          <p>
            <s>{amountText(asked(applied.cpu_millis), asked(applied.memory_mib))}</s>
            {askedByYou && ` · ${NR_REVIEW.PROV_ADDED}`}
          </p>
          <p>
            {NR_REVIEW.NARROWED_APPLIES(amountText(applied.cpu_millis.applied, applied.memory_mib.applied))} {n.tail}
          </p>
        </Notice>
      ))}
      {server.map(({ w, policy }, i) => (
        <Notice key={`${i}:${w}`} tone="warning">
          {reviewWarningText(w)}
          {policy && (
            <>
              <span data-testid="drop-warning-policy" className="mt-1.5 block text-xs text-muted-foreground">
                {policy.source === "deployment" ? NR_REVIEW.POLICY_CONTEXT_DEPLOYMENT : NR_REVIEW.POLICY_CONTEXT(policy.name ?? "")}
              </span>
              <PolicyRemedy policy={{ ...policy, layer: undefined }} line className="mt-0.5" />
            </>
          )}
        </Notice>
      ))}
    </div>
  );
}
