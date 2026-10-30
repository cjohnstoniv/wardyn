/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a Mac runner's macOS VM caps say, as the runner card badge (mock M-R2) and the drawer section
// (mock M-R1) show it. One function decides "offered", so the two never tell different stories.
import type { MacVMCaps } from "../../../lib/types/runner-macvm";
import type { RunnerAmounts } from "../../../lib/types/runners";
import { MACVM_K } from "../../wardyn/copy/runner-macvm";

/** The smallest run the base takes, in the runner's own units, set against what its owner allows one run. */
export function sizeShortfall(caps: MacVMCaps, perRun: RunnerAmounts | undefined): { memory?: { have: number; need: number }; cpu?: { have: number; need: number } } {
  if (!perRun) return {};
  const out: ReturnType<typeof sizeShortfall> = {};
  const needMem = caps.min_memory_mib ?? 0;
  const needCPU = caps.min_cpus ?? 0;
  if (needMem > perRun.memory_mib) out.memory = { have: perRun.memory_mib / 1024, need: needMem / 1024 };
  if (needCPU * 1000 > perRun.cpu_millis) out.cpu = { have: perRun.cpu_millis / 1000, need: needCPU };
  return out;
}

/** The reasons the runner offers no macOS VM at all (the ones that came with a refusal sentence). */
const REFUSED_REASONS = ["ledger_damaged", "memory_below_minimum", "cpu_below_minimum"];

/** Whether the runner's caps say it refuses macOS VMs outright; the console words the refusal from `reason`, never from the runner's text. */
export function refused(caps: MacVMCaps): boolean {
  return REFUSED_REASONS.includes(caps.reason ?? "");
}

export type MacVMBadge = { tone: "neutral" | "warning" | "info"; text: string };

/** The card badge: the count while it offers, "preparing" with the percent, "not offered" otherwise; none without caps. */
export function macVMBadge(caps: MacVMCaps | undefined, active: number | undefined, perRun?: RunnerAmounts): MacVMBadge | null {
  if (!caps) return null;
  const { state, progress } = caps.base;
  if (state === "preparing") return { tone: "info", text: MACVM_K.K02(String(progress ?? 0)) };
  const short = sizeShortfall(caps, perRun);
  if (refused(caps) || state !== "ready" || short.memory || short.cpu) return { tone: "neutral", text: MACVM_K.K03 };
  if (active === undefined) return null;
  return { tone: active >= caps.max ? "warning" : "neutral", text: MACVM_K.K01(String(active), String(caps.max)) };
}
