/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pure side of New run's "Run in" choice: which units a person may pick (their own, the ones they hold a live
// elevation in, the ones they may ask an elevation for), what happens to a pick the units no longer offer, and
// whether a pick is one Review must hold. The reads that feed it are use-run-in.ts.

import type { DefaultPolicyContext } from "../../../../../lib/api/policies";
import type { ElevationOffer, GovernanceRequest, GovernanceRequestList } from "../../../../../lib/types/governance-requests";
import { NR_RUN_IN } from "../../../../wardyn/copy/new-run/run-in";
import type { RunIntent } from "../../draft/run-intent";

export type RunInItem =
  | { group: "yours"; unitId: string; name: string; home: boolean }
  | { group: "elevated"; unitId: string; name: string; endsAt: string }
  | { group: "ask"; unitId: string; name: string; max: string; offer: ElevationOffer };

export interface RunInChoices {
  items: RunInItem[];
  home?: RunInItem;
  /** The person's requests were read; without them only the person's own units are listed. */
  requestsRead: boolean;
}

const byName = (a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name);
const mineElevation = (r: GovernanceRequest) => r.kind === "elevation" && r.mine;
const forUnit = (r: GovernanceRequest, unit: string) => r.unit_id === unit || r.unit_name === unit;

/** The units to pick from; undefined for a server that sends no contexts, where the question is not drawn. */
export function runInChoices(contexts: DefaultPolicyContext[] | undefined, list: GovernanceRequestList | null | undefined, now: number): RunInChoices | undefined {
  if (!contexts) return undefined;
  const live = new Map<string, string>();
  for (const r of list?.requests ?? []) {
    if (mineElevation(r) && r.state === "applied" && r.unit_id && r.ends_at && Date.parse(r.ends_at) > now) live.set(r.unit_id, r.ends_at);
  }
  const yours: Extract<RunInItem, { group: "yours" }>[] = [];
  const elevated: Extract<RunInItem, { group: "elevated" }>[] = [];
  for (const c of contexts) {
    const endsAt = live.get(c.unit_id);
    // The home unit is always the person's own: it is the default, and an elevation never stands in for it.
    if (endsAt && !c.home) elevated.push({ group: "elevated", unitId: c.unit_id, name: c.unit_name, endsAt });
    else yours.push({ group: "yours", unitId: c.unit_id, name: c.unit_name, home: c.home });
  }
  yours.sort((a, b) => Number(b.home) - Number(a.home) || byName(a, b));
  elevated.sort((a, b) => Date.parse(a.endsAt) - Date.parse(b.endsAt));
  const own = new Set(contexts.map((c) => c.unit_id));
  const ask: RunInItem[] = (list?.offered ?? [])
    .filter((o) => !own.has(o.unit_id))
    .map((o): RunInItem => ({ group: "ask", unitId: o.unit_id, name: o.unit_name, max: o.max_elevation, offer: o }))
    .sort(byName);
  const items = [...yours, ...elevated, ...ask];
  return { items, home: items.find((i) => i.group === "yours" && i.home), requestsRead: !!list };
}

/** The item a draft's `run_in` names: by id, or by exact name (a workload start may carry one). */
export const itemFor = (c: RunInChoices | undefined, runIn: string | undefined): RunInItem | undefined =>
  runIn ? c?.items.find((i) => i.unitId === runIn || i.name === runIn) : undefined;

/** Drawn only when there is something to choose: a second unit, a live elevation, or an offer. */
export function shown(c: RunInChoices | undefined): boolean {
  return !!c && (c.items.filter((i) => i.group === "yours").length > 1 || c.items.some((i) => i.group !== "yours"));
}

/**
 * A `run_in` the units still list stays; otherwise the run goes home, with the sentence that says why. The unit is
 * named by its request, else by the name it had when the person picked it, else by the draft's value.
 */
export function reconcileRunIn(
  runIn: string | undefined, c: RunInChoices, list: GovernanceRequestList | null | undefined, homeName: string,
  picked?: { unitId: string; name: string },
): { keep: true } | { keep: false; notice: string } {
  if (!runIn || itemFor(c, runIn)) return { keep: true };
  const had = list?.requests.find((r) => mineElevation(r) && r.state === "applied" && forUnit(r, runIn));
  const name = had?.unit_name ?? (picked?.unitId === runIn ? picked.name : runIn);
  return { keep: false, notice: had ? NR_RUN_IN.ENDED(name, homeName) : NR_RUN_IN.NO_LONGER_IN(name, homeName) };
}

export type RunInBlocker =
  | { kind: "ask"; unitId: string; name: string; offer: ElevationOffer }
  | { kind: "waiting"; unitId: string; name: string };

/** What holds Review for a pick the person can only ask for: the ask, or its wait. An elevated or own unit holds nothing. */
export function runInBlocker(runIn: string | undefined, c: RunInChoices | undefined, list: GovernanceRequestList | null | undefined): RunInBlocker | undefined {
  const item = itemFor(c, runIn);
  if (item?.group !== "ask") return undefined;
  const waiting = list?.requests.some((r) => mineElevation(r) && r.state === "pending" && forUnit(r, item.unitId));
  return waiting ? { kind: "waiting", unitId: item.unitId, name: item.name } : { kind: "ask", unitId: item.unitId, name: item.name, offer: item.offer };
}

export const withRunIn = (intent: RunIntent, unitId: string): RunIntent => ({ ...intent, run_in: unitId });

/** The intent with no `run_in` key at all, so a run that goes home sends today's request byte for byte. */
export function withoutRunIn(intent: RunIntent): RunIntent {
  const { run_in: _dropped, ...rest } = intent;
  return rest;
}
