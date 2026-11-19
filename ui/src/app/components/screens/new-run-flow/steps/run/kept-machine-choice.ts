/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Whether the Run step offers a machine to keep or resume, and what it offers. Only an interactive macOS VM run on the
// person's own Mac can keep one: the site switch is on, no governance profile denies it, and the pool is the person's
// own, a user pool of one Mac. At the person's limit Keep is offered disabled, as resuming still works. Anything else
// draws no section. A machine the draft already holds is cleared only when a server answer proves it invalid, and the
// person is told (useDropProvenMachine); the server's checks at Review and create stand behind this.

import * as React from "react";
import { KEEP_NEW_RUN } from "../../../../wardyn/copy/keep-machine";
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
  /** Days a machine may sit unused before Wardyn deletes it, when the server says. */
  days?: number;
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
  return { mac: offer.runner, gb: gbOf(data.worst_bytes), days: data.idle_days, count: data.count, limit: data.limit, atLimit, machines: resumable(data, offer.runner) };
}

/** Where a kept machine could be offered, if only the read had answered: an interactive macOS VM on the person's own Mac. */
const offerable = (offer: MacOSOffer | undefined, intent: RunIntent): offer is Extract<MacOSOffer, { ok: true }> =>
  intent.machine_kind === "macos_vm" && modeOf(intent) === "interactive" && !!offer?.ok && !!offer.runner && offer.poolScope === "user";

/** The retry of a failed read, when the Run step would have offered a machine: the step says the read failed and offers it. */
export function keptReadRetry(read: KeptMachinesRead, offer: MacOSOffer | undefined, intent: RunIntent): (() => void) | undefined {
  return read.status === "failed" && offerable(offer, intent) ? read.retry : undefined;
}

/**
 * The sentence for a held keep or resume that a server answer proves invalid, else undefined. A read still loading or
 * failed, and an offer still loading, prove nothing: the choice stays (Review holds Launch on a failed read).
 */
export function machineProof(read: KeptMachinesRead, offer: MacOSOffer | undefined, choice: KeptChoice | undefined, intent: RunIntent): string | undefined {
  const asked = intent.machine;
  if (!asked || intent.machine_kind !== "macos_vm" || modeOf(intent) !== "interactive" || offer === undefined) return undefined;
  if (read.status === "absent") return KEEP_NEW_RUN.UNAVAILABLE;
  if (read.status !== "ready") return undefined;
  if (!choice) return KEEP_NEW_RUN.UNAVAILABLE;
  if (asked.keep && choice.atLimit) return KEEP_NEW_RUN.AT_LIMIT;
  if (asked.resume && !choice.machines.some((m) => m.id === asked.resume || m.name === asked.resume)) return KEEP_NEW_RUN.GONE;
  return undefined;
}

/** Clears a keep or resume that machineProof proves invalid, and says so (guided.machineCleared) until the person chooses again. */
export function useDropProvenMachine(read: KeptMachinesRead, offer: MacOSOffer | undefined, choice: KeptChoice | undefined, intent: RunIntent) {
  const dispatch = useNewRunDispatch();
  const proof = machineProof(read, offer, choice, intent);
  React.useEffect(() => {
    if (!proof) return;
    dispatch({ type: "guided", patch: { machineCleared: proof } });
    dispatch({ type: "edit", apply: deleteMachine });
  }, [proof, dispatch]);
}
