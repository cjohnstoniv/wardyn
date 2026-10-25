/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the chosen pool's Macs offer for a macOS VM, as the OS row, the harness list and Review's Limits read it
// (mock M-N1). The org settles this in one function (macOSVMOffer, internal/api/run_placement_capabilities.go) and
// the pool list carries nothing of it yet, so no provider fills the context: without one the macOS option is offered
// whenever the organisation switch is on and the server's own refusal speaks at Launch.

import * as React from "react";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { belowMinimum, type MacOSMinimum } from "../../lenses/limits-lens";
import type { RunIntent } from "../../draft/run-intent";

/** Why a pool's Macs cannot take a macOS VM. `runner` names the Mac only when the pool has exactly one. */
export type MacOSRefusal =
  | { why: "pool_not_offered" }
  | { why: "pool_cant_now" }
  | { why: "not_offered"; runner: string }
  | { why: "preparing"; runner: string; percent: number }
  | { why: "at_limit"; runner: string }
  /** `markId` is set only for a security operator, who may mark the Mac: the runner's id for the trust command. */
  | { why: "not_managed"; runner: string; markId?: string }
  | { why: "no_posture"; runner: string }
  | { why: "no_base"; runner: string; error?: string }
  | { why: "no_room"; runner: string };

export type MacOSOffer =
  | {
      ok: true;
      /** The guest build the base holds, and the smallest run it takes. */
      guestOS: string;
      minCPUs: number;
      minMemoryMiB: number;
      /** The harness ids the base serves; "none" is always among them. */
      harnesses: readonly string[];
      /** Set when the pool has exactly one Mac: the harness refusal names it, else it speaks for the pool. */
      runner?: string;
      /** The agent whose admin disallows sign-ins in a desktop; a macOS desktop cannot block them, so create refuses. */
      signInsOffFor?: string;
    }
  | { ok: false; refusal: MacOSRefusal };

export const MacOSOfferContext = React.createContext<MacOSOffer | undefined>(undefined);
export const useMacOSOffer = (): MacOSOffer | undefined => React.useContext(MacOSOfferContext);

const POSTURE_PATH = "/Library/Application Support/Wardyn/posture.json";

/** The line under the OS row for a refusal: MV-N05 to MV-N10, MV-N14, MV-E03 to MV-E05 and MV-E07. */
export function refusalText(r: MacOSRefusal): string {
  switch (r.why) {
    case "pool_not_offered": return NR_MACHINE.MACOS_POOL_NOT_OFFERED;
    case "pool_cant_now": return NR_MACHINE.MACOS_POOL_CANT_NOW;
    case "not_offered": return NR_MACHINE.MACOS_NOT_OFFERED(r.runner);
    case "preparing": return NR_MACHINE.MACOS_PREPARING(r.runner, r.percent);
    case "at_limit": return NR_MACHINE.MACOS_AT_LIMIT(r.runner);
    case "not_managed": return r.markId ? NR_MACHINE.MACOS_NOT_MANAGED_OPERATOR(r.runner, r.markId) : NR_MACHINE.MACOS_NOT_MANAGED_MEMBER(r.runner);
    case "no_posture": return NR_MACHINE.MACOS_NO_POSTURE(r.runner, POSTURE_PATH);
    case "no_base": return r.error ? NR_MACHINE.MACOS_NO_BASE_ERROR(r.runner, r.error) : NR_MACHINE.MACOS_NO_BASE(r.runner);
    case "no_room": return NR_MACHINE.MACOS_NO_ROOM(r.runner);
  }
}

/** Why the base cannot serve a harness: MV-N11 for a pool of one Mac, MV-N15 for several. Undefined when it can. */
export function harnessRefusal(offer: MacOSOffer | undefined, id: string, display: string): string | undefined {
  if (!offer?.ok || id === "none" || offer.harnesses.includes(id)) return undefined;
  return offer.runner ? NR_MACHINE.MACOS_HARNESS_ONE(offer.runner) : NR_MACHINE.MACOS_HARNESS_POOL(display);
}

/** The smallest run the draft's macOS VM takes, or undefined for any other draft or while the Macs have not said. */
export const macOSMinimumOf = (intent: RunIntent, offer: MacOSOffer | undefined): MacOSMinimum | undefined =>
  intent.machine_kind === "macos_vm" && offer?.ok ? { cpuMillis: offer.minCPUs * 1000, memoryMib: offer.minMemoryMiB } : undefined;

/** MV-N13: the sentence for a macOS VM whose ask was lowered under the base's minimum, else undefined. */
export function macOSMinimumSentence(intent: RunIntent, offer: MacOSOffer | undefined): string | undefined {
  const min = macOSMinimumOf(intent, offer);
  return min && belowMinimum(intent, min) ? NR_MACHINE.MACOS_BELOW_MINIMUM(String(min.cpuMillis / 1000), String(min.memoryMib)) : undefined;
}
