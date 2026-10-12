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
}

export async function loadRoster(): Promise<Roster> {
  const status = await setup.getSetupStatus();
  return { harnesses: status.harnesses ?? [], sets: status.harness_sets ?? [] };
}

export const harnessName = (roster: Roster, id: string): string => roster.harnesses.find((h) => h.id === id)?.display ?? id;

const sameSet = (a: readonly string[], b: readonly string[]) => a.length === b.length && [...a].sort().every((id, i) => id === [...b].sort()[i]);

/**
 * Why a row cannot join the harnesses `chosen`, or undefined when it can. A
 * second harness needs the whole combination in `sets`.
 */
export function unavailableReason(row: SetupHarnessTool, chosen: readonly string[], roster: Roster): string | undefined {
  if (row.enabled === false) return NR_HARNESSES.NOT_ENABLED;
  if (row.installed === false) return NR_HARNESSES.NOT_INSTALLED(row.display);
  if (row.providers_ungranted) return NR_HARNESSES.NO_PROVIDER(row.display);
  if (chosen.length > 0 && !roster.sets.some((s) => sameSet(s, [...chosen, row.id]))) {
    return NR_HARNESSES.NO_COMBINATION(chosen.map((id) => harnessName(roster, id)).join(", "), row.display);
  }
  return undefined;
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
