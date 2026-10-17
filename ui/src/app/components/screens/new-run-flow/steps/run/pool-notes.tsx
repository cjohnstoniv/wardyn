/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the pool area says about a person's own user pools (mock M-POOLS v6 C1 to C5): with no pool
// chosen, why none of theirs can take a run and where to fix it; with one of theirs chosen, the biggest
// run it can take, and that a new run waits while every runner in it is at its limit. Each line comes
// from a fact the pool list carries; none is shown when the fact is absent.
import { Link } from "react-router-dom";
import type { RunnerPoolCatalogueEntry, RunnerPoolRunType } from "../../../../../lib/types/runner-pools";
import { NR_USER_POOLS, RES } from "../../../../wardyn/copy/runner-pools";
import { limitsFor, poolHasNoRoom } from "./pool-model";

const LINK = "text-info underline underline-offset-2 hover:no-underline";
const MY_RUNNER = "/account#my-runner";
const round1 = (n: number) => Number(n.toFixed(1));

function Line({ text, to, link }: { text: string; to: string; link: string }) {
  return (
    <p className="text-xs text-foreground">
      {text}{" "}
      <Link to={to} className={LINK}>
        {link}
      </Link>
    </p>
  );
}

/** The biggest run a pool's runners take, never past what this run type's own maximum allows. */
function biggestRun(pool: RunnerPoolCatalogueEntry, runType: RunnerPoolRunType | undefined): string | undefined {
  const c = pool.capacity;
  if (!c || c.runners === 0) return undefined;
  const eff = limitsFor(pool, runType);
  const within = (v: number, cap?: { value: number; unlimited?: boolean }) => (cap && !cap.unlimited ? Math.min(v, cap.value) : v);
  const big = c.largest_per_run;
  return NR_USER_POOLS.BIGGEST_RUN(
    RES.AMOUNTS(
      round1(within(big.cpu_millis, eff?.cpu_millis.cap) / 1000),
      round1(within(big.memory_mib, eff?.memory_mib.cap) / 1024),
      round1(within(big.disk_mib, eff?.disk_mib.cap) / 1024),
    ),
  );
}

export function PoolNotes({ pools, chosen, runType }: { pools: RunnerPoolCatalogueEntry[]; chosen: RunnerPoolCatalogueEntry | undefined; runType: RunnerPoolRunType | undefined }) {
  if (chosen) {
    if (chosen.scope !== "user") return null;
    const full = poolHasNoRoom(chosen);
    const biggest = biggestRun(chosen, runType);
    if (!full && !biggest) return null;
    return (
      <div data-pool-notes className="flex flex-col gap-1.5">
        {biggest && <p className="text-xs text-muted-foreground">{biggest}</p>}
        {full && (
          <div role="status" className="rounded-lg border border-border bg-surface-2 p-2.5">
            <Line text={NR_USER_POOLS.AT_LIMIT(chosen.name)} to={MY_RUNNER} link={NR_USER_POOLS.SEE} />
          </div>
        )}
      </div>
    );
  }
  const own = pools.filter((p) => p.scope === "user");
  const offline = own.find((p) => p.reason === "runner_pool_no_online_runner");
  let note: React.ReactNode = null;
  if (own.length === 0) note = <Line text={NR_USER_POOLS.NO_POOLS} to="/account/pools/new" link={NR_USER_POOLS.CREATE} />;
  else if (offline) note = <Line text={(offline.my_runners?.length ?? 0) > 1 ? NR_USER_POOLS.OFFLINE_MANY(offline.name) : NR_USER_POOLS.OFFLINE(offline.name)} to={MY_RUNNER} link={NR_USER_POOLS.CHECK} />;
  else if (own.every((p) => p.reason === "runner_pool_no_eligible_member")) note = <Line text={NR_USER_POOLS.NOT_IN_POOL} to={MY_RUNNER} link={NR_USER_POOLS.ADD_TO_POOL} />;
  return note && <div data-pool-notes>{note}</div>;
}
