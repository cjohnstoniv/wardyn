/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Harnesses step reads from GET /setup/status: the roster verbatim and
// the harness sets one declared image carries. A row's state is the server's
// facts and nothing else; absent facts mean unknown and never block.

import { setup } from "../../../../../lib/api/setup";
import type { SetupHarnessTool } from "../../../../../lib/types/setup";
import { NR_HARNESSES } from "../../../../wardyn/copy/new-run/harnesses";

export interface Roster {
  harnesses: SetupHarnessTool[];
  /** Each combination of two or more harnesses one image carries, as sorted ids. */
  sets: string[][];
  /** WARDYN_MULTI_AGENT_TASKS: an Autonomous run may hold several agents. Absent is off. */
  multiAgent?: boolean;
}

export async function loadRoster(): Promise<Roster> {
  const status = await setup.getSetupStatus();
  return { harnesses: status.harnesses ?? [], sets: status.harness_sets ?? [], multiAgent: status.features?.multi_agent_tasks === true };
}

export const harnessName = (roster: Roster, id: string): string => roster.harnesses.find((h) => h.id === id)?.display ?? id;

// The server picks the smallest declared set image that carries every chosen harness (smallestSetImage): a superset serves.
const carriesAll = (set: readonly string[], ids: readonly string[]) => ids.every((id) => set.includes(id));

/**
 * Why a row cannot join the harnesses `chosen`, or undefined when it can. A
 * second harness needs a declared set that carries the whole combination.
 */
export function unavailableReason(row: SetupHarnessTool, chosen: readonly string[], roster: Roster): string | undefined {
  if (row.enabled === false) return NR_HARNESSES.NOT_ENABLED;
  if (row.installed === false) return NR_HARNESSES.NOT_INSTALLED(row.display);
  if (row.providers_ungranted) return NR_HARNESSES.NO_PROVIDER(row.display);
  // "Your own tools" brings no CLI, so the server leaves it out of the set it looks for (harnessCLIs).
  const needs = [...chosen, row.id].filter((id) => id !== "none");
  if (chosen.length > 0 && needs.length > 1 && !roster.sets.some((s) => carriesAll(s, needs))) {
    return NR_HARNESSES.NO_COMBINATION(chosen.map((id) => harnessName(roster, id)).join(", "), row.display);
  }
  return undefined;
}

/**
 * The refusal for the harnesses chosen together, or undefined when one declared image carries them all. It names
 * this card's harness first, then the others, so each card says it in its own order.
 */
export function combinationReason(roster: Roster, ids: readonly string[], id: string): string | undefined {
  const needs = ids.filter((h) => h !== "none");
  if (needs.length < 2 || roster.sets.some((s) => carriesAll(s, needs))) return undefined;
  return NR_HARNESSES.NO_COMBINATION(harnessName(roster, id), ids.filter((h) => h !== id).map((h) => harnessName(roster, h)).join(" and "));
}

/** The hint under a card's select: why the chosen harness cannot run, else whether it holds tools for approval, else that it is available. */
export function harnessHint(roster: Roster, id: string): string | undefined {
  const row = roster.harnesses.find((h) => h.id === id);
  if (!row) return undefined;
  const why = unavailableReason(row, [], roster);
  if (why) return why;
  if (row.approval_hold === false) return id === "codex-cli" ? NR_HARNESSES.CODEX_HINT : NR_HARNESSES.CUSTOM_HINT(row.display);
  return NR_HARNESSES.AVAILABLE(row.display);
}
