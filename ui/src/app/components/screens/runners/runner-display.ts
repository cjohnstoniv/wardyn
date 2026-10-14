/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// How a runner reads, shared by the Admin -> Runners page and the My runner card: the one state word,
// the posture lines, the barrier names and the waiting runner's remaining time. Every posture, barrier
// and folder fact is what the runner reported; absent is "Not reported yet", never a guess.
import { BARRIER_LABEL } from "../../../lib/runner-pool-refusals";
import type { RunnerPosture, RunnerView } from "../../../lib/types/runners";
import { RUNNER } from "../../wardyn/copy/runners";

export type RunnerChipKind = "revoked" | "unclaimed" | "online" | "offline";

/** RN1-D2: one chip per runner. Revoked outranks Not claimed yet, which outranks Online and Offline. */
export function runnerChipKind(r: Pick<RunnerView, "state" | "online">): RunnerChipKind {
  if (r.state === "revoked") return "revoked";
  if (r.state === "unclaimed") return "unclaimed";
  return r.online ? "online" : "offline";
}

export const CHIP_WORD: Record<RunnerChipKind, string> = {
  revoked: RUNNER.STATE_REVOKED,
  unclaimed: RUNNER.STATE_UNCLAIMED,
  online: RUNNER.ONLINE,
  offline: RUNNER.OFFLINE,
};

const SORT_RANK: Record<RunnerChipKind, number> = { unclaimed: 0, online: 1, offline: 2, revoked: 3 };

/** Unclaimed first (it may need someone), then online, offline and revoked, each by name. */
export function sortRunners(rows: RunnerView[]): RunnerView[] {
  return [...rows].sort((a, b) => SORT_RANK[runnerChipKind(a)] - SORT_RANK[runnerChipKind(b)] || a.name.localeCompare(b.name));
}

const OS_NAME: Record<string, string> = { darwin: "macOS", windows: "Windows", linux: "Linux" };

export const osLine = (p: RunnerPosture): string => RUNNER.OS(OS_NAME[p.os] ?? p.os, p.os_version).trim();

/** The drawer and card lines, full words. */
export function postureLines(p: RunnerPosture): string[] {
  return [osLine(p), p.mdm_managed ? RUNNER.MDM_YES : RUNNER.MDM_NO, p.disk_encrypted ? RUNNER.DISK_YES : RUNNER.DISK_NO];
}

/** Barrier names in the order the runner reported them (strongest last). */
export function barrierNames(classes?: RunnerView["classes"]): string {
  return classes?.length ? classes.map((c) => BARRIER_LABEL[c as keyof typeof BARRIER_LABEL] ?? c).join(" · ") : RUNNER.NOT_REPORTED;
}

/** The time left of a waiting runner's claim window or of a token, in the largest whole unit: "23 hours", "40 minutes". */
export function timeLeft(iso: string, now = Date.now()): string {
  const mins = Math.max(0, Math.round((new Date(iso).getTime() - now) / 60000));
  if (mins >= 60) {
    const hours = Math.round(mins / 60);
    return `${hours} ${hours === 1 ? "hour" : "hours"}`;
  }
  return `${mins} ${mins === 1 ? "minute" : "minutes"}`;
}

/** "12 Oct": the day a runner was registered, claimed or revoked; the full time is the title. */
export const shortDate = (iso: string): string => new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short" });
