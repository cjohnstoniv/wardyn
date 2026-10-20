/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Number words for Review's limits: composed only from canon pieces.

import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { ENDS_NONE } from "../../../wardyn/copy/run-lifetime";
import type { HarnessInnerWalls } from "../../../../lib/types/policy";
import type { EffectiveRunLimits } from "../../../../lib/types/runner-pools";
import type { RunIntent } from "../draft/run-intent";

const trimmed = (n: number) => String(Number(n.toFixed(2)));
const grouped = (n: number) => n.toLocaleString("en-US");

export const cpuNumber = (millis: number): string => trimmed(millis / 1000);
export const memoryNumber = (mib: number): string => grouped(mib);
export const cpuText = (millis: number): string => NR_SHARED.CPU(cpuNumber(millis));
export const memoryText = (mib: number): string => NR_SHARED.MIB(memoryNumber(mib));
export const amountText = (cpuMillis: number, memoryMib: number): string => NR_SHARED.JOIN(cpuText(cpuMillis), memoryText(memoryMib));

export function lifetimeText(sec: number): string {
  const hours = sec / 3600;
  return hours === 1 ? NR_SHARED.HOUR_ONE : NR_SHARED.HOURS(trimmed(hours));
}

/**
 * The end a launch will give, as the server resolves it: no end when asked for and allowed, else the asked
 * lifetime (or the default when none was asked, which may itself be no end) held to the cap. Seconds, "none", or undefined when unknown.
 */
export function resolvedLifetime(intent: Pick<RunIntent, "ends_after_sec" | "no_end">, limits: EffectiveRunLimits | undefined): number | "none" | undefined {
  if (intent.no_end && limits?.no_end_allowed) return "none";
  const life = limits?.lifetime_sec;
  // A default of no end, with no end asked for or refused, is what the server gives (run_end_create.go).
  if (!intent.no_end && intent.ends_after_sec === undefined && life?.default.unlimited) return "none";
  const wanted = intent.ends_after_sec ?? (life?.default.unlimited ? undefined : life?.default.value);
  return wanted === undefined || !life || life.cap.unlimited ? wanted : Math.min(wanted, life.cap.value);
}

export const resolvedLifetimeText = (end: number | "none" | undefined): string | undefined =>
  end === "none" ? ENDS_NONE : end === undefined ? undefined : lifetimeText(end);

/** A harness's own walls in the canon's words (#2822): an absent sandbox mode is the harness's own default. */
export function innerWallsText(w: HarnessInnerWalls): string {
  const sandbox = !w.sandbox_mode ? NR_REVIEW.INNER_SANDBOX_OWN
    : w.sandbox_mode === "off" ? NR_REVIEW.INNER_SANDBOX_OFF : NR_REVIEW.INNER_SANDBOX_MODE(w.sandbox_mode);
  const approvals = w.approvals === "harness" ? NR_REVIEW.INNER_APPROVALS_HARNESS
    : w.approvals === "wardyn" ? NR_REVIEW.INNER_APPROVALS_WARDYN : NR_REVIEW.INNER_APPROVALS_NONE;
  return NR_REVIEW.INNER_WALLS(sandbox, approvals);
}
