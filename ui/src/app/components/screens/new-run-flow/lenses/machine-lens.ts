/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The machine of a macOS VM run (`machine`): kept for next time, or resumed from one kept earlier. Absent is today's
// behaviour, the machine deleted when the run ends. Chosen at each launch and never saved in a template. A resumed machine
// keeps its own base, so a base chosen for a new machine goes with the choice to resume.

import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";

/** Delete the machine when the run ends: the absent key. */
export const deleteMachine = (intent: RunIntent): RunIntent => withKeys(intent, { machine: undefined });

/** Keep this run's machine; a name already typed stays. */
export const keepMachine = (intent: RunIntent): RunIntent =>
  withKeys(intent, { machine: { keep: true, ...(intent.machine?.keep && intent.machine.name && { name: intent.machine.name }) } });

/** The name of the kept machine, as typed; empty lets the server pick one. */
export const nameKeptMachine = (intent: RunIntent, name: string): RunIntent =>
  withKeys(intent, { machine: { keep: true, ...(name.trim() && { name }) } });

/** Resume the kept machine with this id; Start fresh stays ticked only while it is the same machine. */
export const resumeMachine = (intent: RunIntent, id: string): RunIntent =>
  withKeys(intent, {
    machine: { resume: id, ...(intent.machine?.resume === id && intent.machine.fresh && { fresh: true }) },
    macos_base: undefined,
  });

/** Start fresh: the machine is reset to its base before the run starts. */
export const startFresh = (intent: RunIntent, fresh: boolean): RunIntent =>
  intent.machine?.resume ? withKeys(intent, { machine: { resume: intent.machine.resume, ...(fresh && { fresh: true }) } }) : intent;
