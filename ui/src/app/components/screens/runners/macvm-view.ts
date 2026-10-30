/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a Mac runner's macOS VM caps say, as the runner card badge (mock M-R2) and the drawer section
// (mock M-R1) show it. One function decides "offered", so the two never tell different stories.
import { shortDate } from "../../../lib/format";
import type { MacVMBase, MacVMCaps, MacVMGuest } from "../../../lib/types/runner-macvm";
import type { RunnerAmounts } from "../../../lib/types/runners";
import { MACVM_K, MACVM_P } from "../../wardyn/copy/runner-macvm";

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

const DAY_MS = 86_400_000;

/** The base's age against the organisation's maximum (site `macos_vm.max_base_age_days`, none when absent or 0). The days are whole, as the organisation counts them. */
export function baseAgeOf(base: MacVMBase, maxDays: number | undefined, now = Date.now()): { created: string; days: number; max?: number; tooOld: boolean } | undefined {
  if (!base.created_at) return undefined;
  const t = Date.parse(base.created_at);
  if (Number.isNaN(t)) return undefined;
  const days = Math.max(0, Math.floor((now - t) / DAY_MS));
  const max = maxDays && maxDays > 0 ? maxDays : undefined;
  return { created: base.created_at, days, max, tooOld: base.state === "ready" && max !== undefined && days > max };
}

/** "3 Nov, 14:20": the day and the time the base was last edited. */
export const editedAtText = (iso: string): string =>
  `${shortDate(iso)}, ${new Date(iso).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false })}`;

/** The guest posture rows the runner read, label and value; a field it did not read, or one a newer runner sends in another word, is left out. */
export function postureRows(g: MacVMGuest | undefined): [string, string][] {
  if (!g) return [];
  const onOff = (v: string | undefined) => (v === "on" ? MACVM_P.P07_ON : v === "off" ? MACVM_P.P07_OFF : undefined);
  const rows: [string, string | undefined][] = [
    [MACVM_P.P02, onOff(g.sip)],
    [MACVM_P.P03, onOff(g.gatekeeper)],
    [MACVM_P.P04, g.profiles === undefined ? undefined : g.profiles > 0 ? MACVM_P.P07_INSTALLED(g.profiles) : MACVM_P.P07_NONE],
    [MACVM_P.P05, g.mdm === "enrolled" ? MACVM_P.P07_ENROLLED : g.mdm === "not_enrolled" ? MACVM_P.P07_NOT_ENROLLED : undefined],
    [MACVM_P.P06, g.org_ca === "trusted" ? MACVM_P.P07_TRUSTED : g.org_ca === "not_trusted" ? MACVM_P.P07_NOT_TRUSTED : undefined],
  ];
  return rows.filter((r): r is [string, string] => r[1] !== undefined);
}
