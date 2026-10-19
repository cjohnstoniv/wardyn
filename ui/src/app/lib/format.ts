/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

export function relativeTime(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime();
  const abs = Math.abs(diff);
  const future = diff < 0;
  const mins = Math.round(abs / 60000);
  if (mins < 1) return future ? "in <1m" : "just now";
  if (mins < 60) return future ? `in ${mins}m` : `${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return future ? `in ${hrs}h` : `${hrs}h ago`;
  const days = Math.round(hrs / 24);
  if (days < 30) return future ? `in ${days}d` : `${days}d ago`;
  const mo = Math.round(days / 30);
  return future ? `in ${mo}mo` : `${mo}mo ago`;
}

export type DurationStyle = "short" | "words" | "clock";

/** One duration, three voices. "short" is the console's default ("1h 2m", "5m 12s"); "words" is prose
 *  for limits and time left, in the largest whole unit ("1 hour", "30 minutes", "2 days"); "clock" is a
 *  player position ("4:07"). */
export function formatDuration(ms: number, style: DurationStyle = "short"): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  if (style === "clock") return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, "0")}`;
  if (style === "words") {
    const unit = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`;
    if (total >= 86400 && total % 86400 === 0) return unit(total / 86400, "day");
    if (total >= 3600 && total % 3600 === 0) return unit(total / 3600, "hour");
    if (total % 60 === 0) return unit(total / 60, "minute");
    return unit(total, "second");
  }
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  return h > 0 ? `${h}h ${m}m` : `${m}m ${total % 60}s`;
}

/** "3 Sep" — a day-of-month and short month, no year or time. Model
 *  connections' "Added {date}" meta line (#592): the exact instant is
 *  already one click away via title=absoluteTime, so the visible stamp stays
 *  short, the same tradeoff relativeTime/absoluteTime make together. */
export function shortDate(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short" });
}

/** "Oct 5, 2026" — a full calendar date, no time of day. */
export function mediumDate(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}

/** "Mon" — the weekday alone. */
export function weekday(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { weekday: "short" });
}

export function absoluteTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** HH:MM:SS clock column for the audit streams (run detail + the Audit screen). */
export function clockTime(iso: string): string {
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** "5:04 PM" — no seconds, unlike clockTime. The `until {time}` egress-scope
 *  copy (deny dialogs, the blast-radius banner, the decided-row badge). */
export function shortTime(iso: string): string {
  return new Date(iso).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}

/** Best-effort human message for a caught value — an Error's message, or its String(). */
export function getErrorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export function fmtBytes(n?: number): string {
  if (n == null) return "—";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

// nonNegativeInt parses a number-input value into the integer a limit field
// stores: blank, NaN, negative or fractional input all collapse to the field's
// zero ("no limit"). `<input type=number min=0>` is only a validation hint —
// "-5" still arrives as a string — and every server reader treats <= 0 as
// unlimited, so a negative saved here would render as a cap that binds nothing.
export function nonNegativeInt(raw: string): number {
  const n = Math.trunc(Number(raw));
  return Number.isFinite(n) && n > 0 ? n : 0;
}
