/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the chosen pool's Macs offer for a macOS VM, as the OS row, the harness list and Review's Limits read it
// (mock M-N1). The org settles this in one function (macOSVMOffer, internal/api/run_placement_capabilities.go) and
// sends it on the pool list (`macos_vm`); MacOSOfferProvider reads the chosen pool's entry into this context. With
// no pool chosen or the field absent there is no offer: the macOS option is offered whenever the organisation
// switch is on and the server's own refusal speaks at Launch.

import * as React from "react";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { MACVM_R } from "../../../../wardyn/copy/runner-macvm";
import { belowMinimum, type MacOSMinimum } from "../../lenses/limits-lens";
import type { RunIntent } from "../../draft/run-intent";
import type { PoolMacOSBase, PoolMacOSVM, PoolMacOSVMRefusal } from "../../../../../lib/types/runner-pools";

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
  | { why: "no_room"; runner: string }
  /** The Mac's base is older than the organisation allows (`days` since it was prepared, `max` allowed), or its runner reports no age (`days` 0). */
  | { why: "base_too_old"; runner: string; days: number; max: number }
  /** A pool of several Macs, one of whose bases is too old or has no age. */
  | { why: "pool_base_too_old"; max: number }
  /** The Mac sent macOS VM caps and refused on its own account; `have` and `need` are GB for memory, CPUs for CPU. */
  | { why: "runner_refused"; runner: string; reason: RunnerRefusedReason; have: number; need: number };

/** The wire's `reason` on a `runner_refused` refusal; any other value reads as "other". */
export type RunnerRefusedReason = "below_min_memory" | "below_min_cpu" | "other";

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
      /** The Macs' other bases besides Standard, by name; none when the pool has only Standard. */
      bases?: readonly PoolMacOSBase[];
      /** When Standard was prepared (the oldest Mac's), for display; never compared with the clock. */
      createdAt?: string;
      /** The caller's governance does not let them pick Standard, and the profile's name for MAC-B05. */
      standardNotAllowed?: boolean;
      profile?: string;
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
    case "base_too_old": return r.days > 0 ? NR_MACHINE.MACOS_BASE_TOO_OLD(r.runner, r.days, r.max) : NR_MACHINE.MACOS_BASE_NO_AGE(r.runner);
    case "pool_base_too_old": return NR_MACHINE.MACOS_BASE_TOO_OLD_POOL(r.max);
    case "runner_refused": {
      const [have, need] = [r.have, r.need].map((n) => String(Number(n.toFixed(1))));
      if (r.reason === "below_min_memory") return MACVM_R.R47(have, need, r.runner);
      if (r.reason === "below_min_cpu") return MACVM_R.R48(have, need, r.runner);
      // MV-N14 is the closest approved "can't take a macOS VM right now" line for a reason with no sentence of its own.
      return NR_MACHINE.MACOS_POOL_CANT_NOW;
    }
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

/** One Mac's refusal, in the runner-specific form (MV-N06 to MV-N10, MV-E03 to MV-E05, MV-E07). */
function runnerRefusal(r: PoolMacOSVMRefusal): MacOSRefusal {
  // "runner_refused" is the wire's `why` for a Mac that sent caps and refused itself, with `reason` (below_min_memory,
  // below_min_cpu or other), `have` and `need`. The sentence is canon by reason; `detail` is never shown.
  if (r.why === "runner_refused") {
    const reason: RunnerRefusedReason = r.reason === "below_min_memory" || r.reason === "below_min_cpu" ? r.reason : "other";
    return { why: "runner_refused", runner: r.runner, reason, have: r.have ?? 0, need: r.need ?? 0 };
  }
  switch (r.why) {
    case "preparing": return { why: "preparing", runner: r.runner, percent: r.progress ?? 0 };
    case "not_managed": return { why: "not_managed", runner: r.runner, markId: r.runner_id };
    case "no_base": return { why: "no_base", runner: r.runner, error: r.error };
    case "base_too_old": return { why: "base_too_old", runner: r.runner, days: r.have ?? 0, max: r.need ?? 0 };
    default: return { why: r.why, runner: r.runner };
  }
}

/**
 * The offer a pool's `macos_vm` makes, or undefined while it says nothing (no pool, or the field absent). A pool of
 * one Mac names it (`runner`) and gets that Mac's own refusal; a pool of several gets the pool form, MV-N05 when any
 * Mac does not offer macOS VMs at all, else MV-N14.
 */
export function macOSOfferOf(pool: PoolMacOSVM | undefined, profile?: string): MacOSOffer | undefined {
  if (!pool) return undefined;
  if (pool.offered) {
    return {
      ok: true,
      guestOS: pool.guest_os ?? "",
      minCPUs: pool.min_cpus ?? 0,
      minMemoryMiB: pool.min_memory_mib ?? 0,
      harnesses: pool.harnesses?.includes("none") ? pool.harnesses : ["none", ...(pool.harnesses ?? [])],
      runner: pool.runner,
      bases: pool.bases ?? [],
      createdAt: pool.created_at,
      standardNotAllowed: !!pool.standard_not_allowed,
      profile,
    };
  }
  const refusals = pool.refusals ?? [];
  if (pool.runner && refusals.length === 1) return { ok: false, refusal: runnerRefusal(refusals[0]) };
  const notOffered = refusals.length === 0 || refusals.some((r) => r.why === "not_offered");
  const tooOld = refusals.find((r) => r.why === "base_too_old");
  if (!notOffered && tooOld) return { ok: false, refusal: { why: "pool_base_too_old", max: tooOld.need ?? 0 } };
  return { ok: false, refusal: { why: notOffered ? "pool_not_offered" : "pool_cant_now" } };
}
