/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Whether the Run step offers a machine to keep or resume, and what it offers. Only an interactive macOS VM run on the
// person's own Mac can keep one: the site switch is on, no governance profile denies it, and the pool is the person's
// own, a user pool of one Mac. At the person's limit Keep is offered disabled, as resuming still works. Anything else
// draws no section, and a machine the draft already holds is dropped (useDropUnofferedMachine); the server's checks at
// Review and create stand behind this.

import * as React from "react";
import { KEPT_MACHINE_REASON } from "../../../../../lib/kept-machine-refusals";
import { gbOf } from "../../../../wardyn/copy/keep-machine";
import type { KeptMachine, MyMachines } from "../../../../../lib/types";
import type { RunIntent } from "../../draft/run-intent";
import { deleteMachine } from "../../lenses/machine-lens";
import { modeOf } from "../../lenses/shape-lens";
import { useNewRunDispatch } from "../../new-run-route";
import type { KeptMachinesRead } from "./kept-machines-provider";
import type { MacOSOffer } from "./macos-offer";

export interface KeptChoice {
  /** The Mac the run starts on: the one Mac of the person's pool. */
  mac: string;
  /** The most disk one new kept machine takes, whole GB. */
  gb: number;
  count: number;
  limit: number;
  /** Keep is offered but disabled: the person keeps their limit already. */
  atLimit: boolean;
  /** The machines Resume offers, in the server's order: every one that is not being deleted, on this Mac. */
  machines: KeptMachine[];
}

/** The machines a resume may name here: kept or held by a run, never one being deleted, and on this run's Mac. */
export const resumable = (data: MyMachines, mac: string): KeptMachine[] =>
  data.machines.filter((m) => m.state !== "deleting" && (!m.runner_name || m.runner_name === mac));

export function keptChoiceOf(read: KeptMachinesRead, offer: MacOSOffer | undefined, intent: RunIntent): KeptChoice | undefined {
  if (read.status !== "ready" || intent.machine_kind !== "macos_vm" || modeOf(intent) !== "interactive") return undefined;
  if (!offer?.ok || !offer.runner || offer.poolScope !== "user") return undefined;
  const { data } = read;
  const atLimit = data.reason === KEPT_MACHINE_REASON.PERSON_LIMIT;
  if (!data.allowed && !atLimit) return undefined;
  return { mac: offer.runner, gb: gbOf(data.worst_bytes), count: data.count, limit: data.limit, atLimit, machines: resumable(data, offer.runner) };
}

/**
 * Takes a keep or resume out of the draft once the read says this run cannot have one: the draft would only be refused.
 * While the read is still loading, nothing is judged.
 */
export function useDropUnofferedMachine(read: KeptMachinesRead, choice: KeptChoice | undefined, intent: RunIntent) {
  const dispatch = useNewRunDispatch();
  const held = !!intent.machine;
  const gone = held && read.status !== "loading" && (!choice || (intent.machine?.keep && choice.atLimit) || (!!intent.machine?.resume && !choice.machines.some((m) => m.id === intent.machine?.resume)));
  React.useEffect(() => {
    if (gone) dispatch({ type: "edit", apply: deleteMachine });
  }, [gone, dispatch]);
}
