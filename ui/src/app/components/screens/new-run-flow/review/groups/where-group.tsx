/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Button } from "../../../../ui/button";
import { Label } from "../../../../ui/label";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { ConfinementClass } from "../../../../../lib/types/runs";
import { chooseBarrier, choosePool } from "../../lenses/run-lens";
import { barrierRows, poolChoices, selectedPoolId, usablePools } from "../../steps/run/pool-model";
import { loadPools, usePools } from "../../steps/run/pool-source";
import { BarrierChip } from "../barrier-chip";
import { draftSource } from "../provenance";
import { ReviewGroup } from "../review-group";
import { onlineRunners } from "../review-seams";
import { SELECT_CLASS } from "../save-as-dialog";
import type { GroupProps } from "./group-props";

// The pool line names the pool's kind (canon: new-run-canon.md WHERE_DETAIL, pool words).
const POOL_KIND_LABEL = { organization: "Organization pool", user: "User pool" } as const;
const BARRIER_NAME: Record<ConfinementClass, string> = { CC1: NR_SHARED.BARRIER_FENCE, CC2: NR_SHARED.BARRIER_WALL, CC3: NR_SHARED.BARRIER_VAULT };

/** Change, in place: the pool and the barrier as the Run step offers them (its pool list and barrier rows), then Done. */
function WhereEditor({ draft, dispatch, close }: Pick<GroupProps, "draft" | "dispatch"> & { close: () => void }) {
  const { intent } = draft;
  const pools = usePools(loadPools, intent.experience);
  const list = pools.status === "ready" ? pools.list : undefined;
  const usable = list ? usablePools(list) : [];
  const poolId = list ? selectedPoolId(intent, list) : "";
  const { options, pick } = barrierRows(usable.find((p) => p.id === poolId));
  const cc = intent.confinement_class ?? pick?.cc ?? "";
  return (
    <div className="flex flex-wrap items-end gap-2.5">
      <div className="flex min-w-40 flex-1 flex-col gap-2">
        <Label htmlFor="nr-review-pool">{NR_REVIEW.POOL_LABEL}</Label>
        <select id="nr-review-pool" className={SELECT_CLASS} value={poolId} disabled={!list}
          onChange={(e) => {
            const id = e.target.value;
            dispatch({ type: "edit", apply: (i) => choosePool(i, id) });
          }}>
          {!poolId && <option value="" disabled hidden />}
          {poolChoices(usable, intent.experience).map((c) => <option key={c.id} value={c.id} disabled={c.disabled}>{c.label}</option>)}
        </select>
      </div>
      <div className="flex min-w-40 flex-1 flex-col gap-2">
        <Label htmlFor="nr-review-barrier">{NR_REVIEW.BARRIER_LABEL}</Label>
        <select id="nr-review-barrier" className={SELECT_CLASS} value={cc} disabled={!list}
          onChange={(e) => {
            const chosen = e.target.value as ConfinementClass;
            dispatch({ type: "edit", apply: (i) => chooseBarrier(i, chosen) });
          }}>
          {!cc && <option value="" disabled hidden />}
          {options.map((o) => (
            <option key={o.cc} value={o.cc} disabled={!o.available}>
              {o.available || !o.reason ? BARRIER_NAME[o.cc] : NR_SHARED.JOIN(BARRIER_NAME[o.cc], o.reason)}
            </option>
          ))}
        </select>
      </div>
      <Button type="button" variant="outline" onClick={close}>{NR_REVIEW.DONE}</Button>
      {pools.status === "error" && <p role="alert" className="basis-full text-xs text-danger">{pools.message}</p>}
    </div>
  );
}

export function WhereGroup({ draft, pre, docRows, dispatch }: GroupProps) {
  const pool = pre?.runner_pool;
  const slots = pre?.runner_pool_slots;
  const online = onlineRunners(pre);
  const kind = pool ? POOL_KIND_LABEL[pool.scope] : undefined;
  const poolLine = !kind ? undefined
    : !slots ? kind
    : slots.max === undefined ? NR_SHARED.JOIN(kind, NR_RUN.RUNNING_ONLY(slots.used))
    : NR_REVIEW.WHERE_DETAIL(kind, slots.used, slots.max);
  const detail = online === undefined ? poolLine : poolLine ? NR_SHARED.JOIN(poolLine, NR_REVIEW.WHERE_ONLINE(online)) : NR_REVIEW.WHERE_ONLINE(online);
  const cc = pre?.enforced_confinement_class ?? draft.intent.confinement_class;
  const provenance = pool
    ? pool.selection === "explicit" ? NR_REVIEW.PROV_SELECTED : pool.selection === "organisation_default" ? NR_REVIEW.PROV_ORG_DEFAULT : undefined
    : draftSource(draft, ["runner_pool_id"], NR_REVIEW.PROV_SELECTED);
  return (
    <ReviewGroup
      id="where"
      label={NR_REVIEW.GROUP_WHERE}
      value={pool && NR_REVIEW.WHERE_VALUE(pool.name)}
      mark={cc && <BarrierChip cc={cc} />}
      detail={detail}
      provenance={provenance}
      docRows={docRows}
      editor={(close) => <WhereEditor draft={draft} dispatch={dispatch} close={close} />}
    />
  );
}
