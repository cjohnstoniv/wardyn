/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { useKeptMachines } from "../../steps/run/kept-machines-provider";
import { keptReviewLines } from "../kept-machine-lines";
import * as React from "react";
import { Button } from "../../../../ui/button";
import { Label } from "../../../../ui/label";
import { MAC_BASES } from "../../../../wardyn/copy/mac-bases";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_RUN_IN } from "../../../../wardyn/copy/new-run/run-in";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_WINDOW } from "../../../../wardyn/copy/new-run/window";
import { Chip } from "../../../../wardyn/primitives";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import type { ConfinementClass } from "../../../../../lib/types/runs";
import { isVMKind } from "../../../../../lib/types/machine-kind";
import { chooseBarrier, choosePool } from "../../lenses/run-lens";
import { barrierRows, poolChoices, selectedPoolId, usablePools } from "../../steps/run/pool-model";
import { loadPools, usePools } from "../../steps/run/pool-source";
import { useMacOSOffer } from "../../steps/run/macos-offer";
import { itemFor } from "../../steps/run/run-in-model";
import { useRunIn } from "../../steps/run/use-run-in";
import { weekdayClock } from "../../../run-detail/run-ends-row";
import { chooseWindowIn, macWindowOn } from "../../steps/run/window-choice";
import { WindowCheckbox, WindowCleared } from "../../steps/run/window-option";
import { BarrierChip } from "../barrier-chip";
import { draftSource } from "../provenance";
import { BlockedGroups, ReviewGroup } from "../review-group";
import { onlineRunners } from "../review-seams";
import { SELECT_CLASS } from "../save-as-dialog";
import type { GroupProps } from "./group-props";

// The pool line names the pool's kind (canon: new-run-canon.md WHERE_DETAIL, pool words).
const POOL_KIND_LABEL = { organization: "Organization pool", user: "User pool" } as const;
const BARRIER_NAME: Record<ConfinementClass, string> = { CC1: NR_SHARED.BARRIER_FENCE, CC2: NR_SHARED.BARRIER_WALL, CC3: NR_SHARED.BARRIER_VAULT };

/**
 * Change, in place: the pool and the barrier as the Run step offers them (its pool list and barrier rows), then Done.
 * A macOS VM has no barrier to choose; while the site switch is on, the window on the Mac takes its place.
 */
function WhereEditor({ draft, dispatch, close }: Pick<GroupProps, "draft" | "dispatch"> & { close: () => void }) {
  const { intent } = draft;
  const { status } = useShellSetupStatus();
  const windowId = React.useId();
  const macOS = intent.machine_kind === "macos_vm";
  const vm = isVMKind(intent.machine_kind);
  const windowOn = macOS && macWindowOn(status);
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
      {windowOn ? (
        <div role="group" aria-labelledby={`${windowId}-label`} className="flex min-w-40 flex-1 flex-col gap-2">
          <span id={`${windowId}-label`} className="text-sm font-medium">{NR_WINDOW.GROUP}</span>
          <WindowCheckbox id={windowId} chosen={!!intent.desktop_window} onChoose={(on) => chooseWindowIn(dispatch, on)} />
        </div>
      ) : vm ? (
        // A virtual machine's barrier is fixed (the runner enforces CC3 for a Linux or macOS VM), as the Run step says:
        // no container barrier to pick.
        <dl data-testid="nr-review-vm-barrier" className="flex min-w-40 flex-1 flex-col gap-1 text-sm">
          <dt className="font-medium">{NR_MACHINE.BARRIER_LABEL}</dt>
          <dd>{NR_MACHINE.BARRIER_VM}</dd>
          <dd className="text-xs text-muted-foreground">{NR_MACHINE.BARRIER_VM_WHY}</dd>
        </dl>
      ) : (
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
      )}
      <Button type="button" variant="outline" onClick={close}>{NR_REVIEW.DONE}</Button>
      {pools.status === "error" && <p role="alert" className="basis-full text-xs text-danger">{pools.message}</p>}
    </div>
  );
}

export function WhereGroup({ draft, pre, docRows, dispatch }: GroupProps) {
  const pool = pre?.runner_pool;
  // A blocker that points at this row while no pool is known (the pool-required refusal): the row says so (#3043).
  const blocked = React.useContext(BlockedGroups).has("where");
  const noPool = !pool && !draft.intent.runner_pool_id && blocked;
  const slots = pre?.runner_pool_slots;
  const online = onlineRunners(pre);
  const kind = pool ? POOL_KIND_LABEL[pool.scope] : undefined;
  const poolLine = !kind ? undefined
    : !slots ? kind
    : slots.max === undefined ? NR_SHARED.JOIN(kind, NR_RUN.RUNNING_ONLY(slots.used))
    : NR_REVIEW.WHERE_DETAIL(kind, slots.used, slots.max);
  const poolDetail = online === undefined ? poolLine : poolLine ? NR_SHARED.JOIN(poolLine, NR_REVIEW.WHERE_ONLINE(online)) : NR_REVIEW.WHERE_ONLINE(online);
  // A macOS VM on a base other than Standard names it on its own line under the detail (board J6, Review row).
  const base = draft.intent.machine_kind === "macos_vm" ? draft.intent.macos_base : undefined;
  // The window on the Mac, when chosen, is one more line (W2-07, or W2-08 for a pool of several Macs); a choice the
  // server cleared says so where it was made.
  const offer = useMacOSOffer();
  const runner = offer?.ok ? offer.runner : undefined;
  const windowLine = draft.intent.desktop_window ? (runner ? NR_WINDOW.REVIEW_ONE_MAC(runner) : NR_WINDOW.REVIEW_SEVERAL_MACS) : undefined;
  const cleared = !draft.intent.desktop_window ? draft.guided.windowCleared : undefined;
  const keptLines = keptReviewLines(draft.intent, useKeptMachines(), runner);
  const keptCleared = !draft.intent.machine ? draft.guided.machineCleared : undefined;
  // The unit the run runs in, first among the detail lines: its end too, for a unit the person holds an elevation in.
  const unit = itemFor(useRunIn().choices, draft.intent.run_in);
  const detail = !unit && !base && !windowLine && !cleared && !keptLines && !keptCleared ? poolDetail : (
    <>
      {unit && (
        <span className="flex flex-wrap items-center gap-2">
          <span>{NR_RUN_IN.REVIEW_ROW(unit.name)}</span>
          {unit.group === "elevated" && (
            <>
              <Chip tone="info">{NR_RUN_IN.CHIP_ELEVATED}</Chip>
              <span>{NR_RUN_IN.REVIEW_ENDS(weekdayClock(unit.endsAt))}</span>
            </>
          )}
        </span>
      )}
      {poolDetail && (unit ? <span className="block">{poolDetail}</span> : poolDetail)}
      {base && <span className="block">{MAC_BASES.B16(base)}</span>}
      {keptCleared && <span className="block">{keptCleared}</span>}
      {keptLines && (
        <>
          <span className="block">{keptLines.machine}</span>
          <span className="block">{keptLines.detail}</span>
        </>
      )}
      {windowLine && <span className="block">{windowLine}</span>}
      {cleared && <span className="mt-1 block"><WindowCleared text={cleared} /></span>}
    </>
  );
  const cc = pre?.enforced_confinement_class ?? draft.intent.confinement_class;
  const provenance = pool
    ? pool.selection === "explicit" ? NR_REVIEW.PROV_SELECTED : pool.selection === "organisation_default" ? NR_REVIEW.PROV_ORG_DEFAULT : undefined
    : draftSource(draft, ["runner_pool_id"], NR_REVIEW.PROV_SELECTED);
  return (
    <ReviewGroup
      id="where"
      label={NR_REVIEW.GROUP_WHERE}
      value={pool ? NR_REVIEW.WHERE_VALUE(pool.name) : noPool ? <span className="font-normal text-muted-foreground">{NR_REVIEW.WHERE_NONE}</span> : undefined}
      mark={cc && <BarrierChip cc={cc} />}
      detail={detail}
      provenance={provenance}
      docRows={docRows}
      editor={(close) => <WhereEditor draft={draft} dispatch={dispatch} close={close} />}
    />
  );
}
