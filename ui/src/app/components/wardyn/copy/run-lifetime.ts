/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5) — the run lifetime canon strings: Ends, the 24h/1h/
// 10min warnings, ended/lost/revive, pausing and the wait row. Every literal
// here is copied verbatim from the owner-approved packet
// (wardyn-archive/mock-08/long-holds-packet.html) and long-holds-design.md
// §6 — a value that differs per run (a date, a count, a duration) is a
// function argument, never re-worded.

// ---- Ends (design.md §2.3) ----

export function endsValue(dateText: string, relText: string): string {
  return `Ends ${dateText} (${relText})`;
}
export const ENDS_NONE = "No end";
export function endsHint(days: number): string {
  return `You can set it up to ${days} day${days === 1 ? "" : "s"} ahead and extend it any time.`;
}
// F11 (PR #1317 review): the mock's own admin-set column (packet:249-251)
// shows the value line ("Ends Tue 18:00 (in 8 hours)") ABOVE this hint, so the
// hint itself never repeats the date — only the caller (run-ends-row.tsx),
// which already renders that value line right beside this one, would.
export function endsLocked(days: number): string {
  return `You can extend it up to ${days} day${days === 1 ? "" : "s"} ahead; your admin sets the rest.`;
}
export function endsCapped(days: number): string {
  return `That's as far as your admin allows (${days} day${days === 1 ? "" : "s"}). You can extend again later.`;
}
// ENDS_CAPPED_LOOSENED (packet:268, design:312) is defined for completeness
// but UNUSED: nothing on the wire distinguishes "still capped by the live
// policy" from "capped by a now-stale captured value the admin has since
// loosened" — run.run_limits is captured once at create and never updated
// (run_limits_reclamp.go's own comment: "Loosening never reaches a live
// run"). Filed as issue #1322 (a companion to #1319/#1320) rather than
// guessed at.
export const ENDS_CAPPED_LOOSENED =
  "Your admin loosened this after the run started. Start a new run to get the new limit.";
export function endsTightened(dateText: string): string {
  return `Your admin shortened the limit. This run now ends ${dateText}.`;
}
export const ENDS_EXTEND = "Extend";
export const ENDS_EXTEND_1_DAY = "1 more day";
export const ENDS_EXTEND_1_WEEK = "1 more week";
export function endsExtendAsFarAsAllowed(days: number): string {
  return `As far as allowed (${days} day${days === 1 ? "" : "s"})`;
}
export const ENDS_CHANGE = "Change…";
export const ENDS_SET_AN_END = "Set an end…";
export const ENDS_NO_END_OPTION = "No end";
// M2 (packet:246): the "No end" state's own hint, shown only where allow_no_end
// is true (the state it always describes — an admin-locked run never reaches
// "No end" in the first place, since Change… is how anyone gets there).
export const NO_END_HINT =
  "Keeps going until you or an admin ends it. It pauses when nobody is here and keeps its memory.";

// ---- Warnings (design.md §2.3, mock "1 hour before") ----

export type EndsWarningStage = "24h" | "1h" | "10m";

// Every warning fires once the run's remaining time crosses its threshold;
// the 24h stage additionally needs a lease longer than 2 days (design.md
// §2.3's "leases over 2 days") so an 8-hour run doesn't get a redundant 24h
// notice for a lease it never had. approxLeaseMs is created_at -> ends_at:
// the closest proxy this console has to "how far ahead was this end set",
// since neither the original set time nor the pre-extension value is on the
// wire.
export function endsWarningStage(
  endsAtIso: string | null | undefined,
  nowMs: number,
  approxLeaseMs: number,
): EndsWarningStage | null {
  if (!endsAtIso) return null;
  const msLeft = Date.parse(endsAtIso) - nowMs;
  if (msLeft <= 0) return null;
  const minLeft = msLeft / 60000;
  if (minLeft <= 10) return "10m";
  if (minLeft <= 60) return "1h";
  if (minLeft <= 24 * 60 && approxLeaseMs > 2 * 24 * 60 * 60 * 1000) return "24h";
  return null;
}

const WARNING_REL_TEXT: Record<EndsWarningStage, string> = {
  "24h": "24 hours",
  "1h": "1 hour",
  "10m": "10 minutes",
};

// The board/table row chip (design.md §2.3: "a board chip 'Ends in 1 hour'")
// and the warning banner's own title share this exact phrase.
export function endsWarningWord(stage: EndsWarningStage): string {
  return `Ends in ${WARNING_REL_TEXT[stage]}`;
}
export function warnTitle(stage: EndsWarningStage): string {
  return `This run ends in ${WARNING_REL_TEXT[stage]}`;
}
export function warnBody(dateText: string): string {
  return `At ${dateText} the run stops. Its files are kept for 7 days; push or save anything you need before then.`;
}
export const WARN_EXTEND_1_DAY = "Extend 1 day";
export const WARN_CHANGE_END = "Change end…";
export const WARN_DISMISS = "Dismiss";

// ---- Ended (design.md §2.3, mock "after the end") ----

export const ENDED_TITLE = "This run ended at its end time";
export function endedBody(dateText: string): string {
  return `It has no network. Its files are kept until ${dateText}. Extend to revive it.`;
}
// F4 (PR #1317 review): the grace deadline is lost_at + Config.EndedRunGrace,
// and EndedRunGrace is server-only config, never on the wire (filed as
// #1320). Rendering endedBody(lost_at) would print the moment the run
// ENDED, not the moment its files go away — a wrong, not just a missing,
// date. This is the interim wording until #1320 adds the real one.
export const ENDED_BODY_NO_DATE = "It has no network. Extend to revive it before its files are cleaned up.";
export const ENDED_EXTEND_AND_REVIVE = "Extend and revive";
// F9 (PR #1317 review): a task run's own agent ran once, at dispatch — the
// server always refuses reviveEligible's "task run's agent cannot be started
// again" for an ended, non-interactive run (run_revive.go). Only Extend
// (never revive) is offered for one.
export const ENDED_EXTEND_ONLY = "Extend";
export const END_RUN = "End run";

// ---- Lost and revive (design.md §4.1, mock "Paused, and lost then revived") ----

export const LOST_TITLE = "This run's sandbox stopped";
export function lostBody(dateText: string): string {
  return `The machine it ran on restarted, so the run has no network. Its files are kept until it ends (${dateText}).`;
}
export const LOST_BODY_CLAUDE =
  "Revive starts it again and continues the Claude Code conversation. Programs that were running are not restored.";
export const LOST_BODY_OTHER = "Revive starts it again with its files. The agent starts a new session.";
export const LOST_OUTAGE =
  "Wardyn was unreachable for over an hour, so this run's network was stopped. Revive reconnects it; the terminal stays as it was.";
// M4 (design:336, packet:287): no k8s revive in 0.8 (L6) — a node-restarted
// run only ever keeps what a drive holds.
export const LOST_K8S = "The node it ran on restarted. Files on your drive are kept; the rest is gone.";
export const REVIVE = "Revive";
export const REVIVING = "Reviving… (about a minute)";
export function revivePolicy(hostsBlocked: number): string {
  return `Policy updated at revive: ${hostsBlocked} host${hostsBlocked === 1 ? "" : "s"} now blocked.`;
}

// ---- Paused (design.md §3, mock "Paused, and lost then revived") ----

export const PAUSED_WAITING_TITLE = "Paused while waiting for approval";
export function pausedWaitingBody(untilText: string): string {
  return `Resumes when someone decides, or when you type in the terminal. The request stays open until ${untilText}.`;
}
export function pausedIdleTitle(minutes: number): string {
  return `Paused — nobody's been here for ${minutes} minute${minutes === 1 ? "" : "s"}`;
}
export const PAUSED_IDLE_BODY = "It stopped using CPU and kept its memory. Type or press Resume; it's back in a second or two.";
export const RESUME_NOW = "Resume now";

// ---- Wait row (design.md §2.3) ----

export const WAIT_LABEL = "If this run waits for a decision";
export function waitValueHours(hours: number): string {
  return `Keep it for up to ${hours} hour${hours === 1 ? "" : "s"}`;
}
export function waitLocked(hours: number): string {
  return `Kept for up to ${hours} hour${hours === 1 ? "" : "s"}. Your admin sets this.`;
}

// F14 (PR #1317 review, run_pause.go's pauseDelayFloor): the server never
// pauses an idle run sooner than 630s, even when a profile sets less — the
// banner must say the time it ACTUALLY pauses at, not the raw setting.
export const PAUSE_IDLE_FLOOR_SEC = 630;

// ---- Admin: older limits + bulk restart (design.md §2.2, mock "Admin") ----

export const OLDER_LIMITS_CHIP = "Older limits";
export const RESTART_WITH_CURRENT_LIMITS = "Restart with current limits";
export const RESTART_HINT =
  "Restarts each run's network proxy with its owner's current limits. Terminals stay open; requests in flight fail once.";
