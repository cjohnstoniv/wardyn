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
