/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A run document that says its run mode in the older fields (agent, task,
// interactive and the rest) and in no carrier. The console sends the contract
// marker with every run, so the server refuses the older fields there, and no
// Guided step removes them. Committing such a document puts the same run in the
// carriers instead, where Guided setup can change it and Review can send it.

import { LEGACY_RUN_MODE_FIELDS, mapRunMode, type RunModeBody, type RunModeCarriers } from "../../../../lib/run-mode-map";
import type { RunShape } from "../../../../lib/types/runs";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "../lenses/lens-keys";

/** The shape the carriers say, as a clone names it (prefill-seed.ts). */
function shapeOf(c: RunModeCarriers): RunShape {
  if (c.experience === "background") return c.workload?.kind === "command" ? "application" : "agent_task";
  return c.startup?.kind === "harness" ? "agent_task" : "environment";
}

const NO_LEGACY = Object.fromEntries(LEGACY_RUN_MODE_FIELDS.map((k) => [k, undefined])) as Partial<RunIntent>;

/**
 * The intent with its older run-mode fields put in the carriers, or the intent as it is. Converted only
 * when it names no carrier and mapRunMode can say the same run. A run document with no `interactive` is a
 * background run, as `wardyn run --file` reads it and as the SDK writes one (it leaves a false
 * `interactive` out). A template may leave a background run's text, and its agent, for later (textLater).
 * Otherwise nothing changes, and Review's refusal names the older field to change in the document.
 */
export function withLegacyRunModeInCarriers(intent: RunIntent, textLater = false): RunIntent {
  if (intent.experience !== undefined || intent.workload !== undefined || intent.tools !== undefined || intent.startup !== undefined) return intent;
  const carriers = mapRunMode(intent as RunModeBody, textLater);
  if (!carriers) return intent;
  const { experience, workload, tools, startup } = carriers;
  return withKeys(intent, { ...NO_LEGACY, shape: shapeOf(carriers), experience, workload, tools, startup });
}

/**
 * At the ask: a draft that still holds older run-mode fields and no `interactive` (a template leaves the run mode
 * to the person) is converted with what the person's choice in Guided setup says, as a template that states its
 * run mode is when it is seeded. Only the fields the draft leaves out are taken from the choice. The run-mode
 * fields Guided wrote for the draft so far are written again; the draft is kept as it is when it holds tools, or
 * when mapRunMode cannot say the run.
 */
export function withAskedRunMode(intent: RunIntent, asked: Pick<RunModeBody, "interactive" | "interactive_start" | "task_mode">): RunIntent {
  const record = intent as Record<string, unknown>;
  // An empty or false older field says nothing, and the server takes it beside the run-mode fields (the SDK writes an empty `agent`).
  const says = (v: unknown) => v !== undefined && v !== "" && v !== false;
  if (intent.interactive !== undefined || !LEGACY_RUN_MODE_FIELDS.some((k) => says(record[k]))) return intent;
  const said = withKeys({ ...asked, ...intent } as RunIntent, { experience: undefined, workload: undefined, startup: undefined });
  const carried = withLegacyRunModeInCarriers(said, true);
  return carried.experience === undefined ? intent : carried;
}
