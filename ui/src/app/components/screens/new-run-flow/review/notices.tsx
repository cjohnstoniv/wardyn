/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The true notices: the pool is at its limit, a size was narrowed, and the server's own preflight warnings.

import { Notice } from "../../../wardyn/notice";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { RUN_WAIT } from "../../../../lib/runner-pool-refusals";
import type { AppliedAmount, AppliedResources } from "../../../../lib/types/new-run-contract";
import type { LimitSource } from "../../../../lib/types/runner-pools";
import { amountText } from "./format";
import type { PoolSlotsFact } from "./review-seams";

/** composer.WarnResourcesCapped. A CPU or memory cut is said again, with its source, by the narrowed notice; a pids or disk cut is not. */
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
}

export function Notices({ poolName, slots, applied, askedByYou, warnings }: NoticesProps) {
  const full = slots?.verdict === "waits" && slots.max !== undefined && !!poolName;
  const found = applied
    ? [narrowing(applied.cpu_millis, poolName), narrowing(applied.memory_mib, poolName)].filter((n): n is Narrowing => n !== undefined)
    : [];
  const distinct = found.filter((n, i) => found.findIndex((m) => m.key === n.key) === i);
  const server = (warnings ?? []).filter((w) => w !== RESOURCES_CAPPED || distinct.length === 0);
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
      {server.length > 0 && (
        <Notice tone="warning" title={NR_REVIEW.WARNINGS_TITLE}>
          <ul className="list-disc pl-4">
            {server.map((w, i) => <li key={`${i}:${w}`}>{w}</li>)}
          </ul>
        </Notice>
      )}
    </div>
  );
}
