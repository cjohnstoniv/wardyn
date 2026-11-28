/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunsListFilteredResult } from "../../../lib/api/runs";
import { isTerminalRunState, type AgentRun } from "../../../lib/types";

/** How far before the newest end time the board holds a poll's cut sits: an end written in a transaction that
 * committed late, a replica clock behind the one that stamped it, and an ended run's own changes in the two minutes
 * after it ends (the stop confirm, the folder's fate) are all read again. */
export const DELTA_OVERLAP_MS = 120_000;

/** The end time the server orders and cuts ended runs by (runEndTimeSQL, internal/store/store_runs_filtered.go): a
 * lease-ended run's lost_at, every other run's ended_at. */
export function runEndTime(r: AgentRun): string | undefined {
  return r.lost_reason === "ended" ? r.lost_at : r.ended_at;
}

/** The ended_after of a poll after `last`: the newest end time it holds less DELTA_OVERLAP_MS, in whole seconds.
 * Undefined when it holds no ended run, so the poll reads whole. */
export function boardCut(last: RunsListFilteredResult): string | undefined {
  let newest = -Infinity;
  for (const r of last.runs) {
    const end = runEndTime(r);
    if (end) newest = Math.max(newest, Date.parse(end));
  }
  if (!Number.isFinite(newest)) return undefined;
  return new Date(Math.floor((newest - DELTA_OVERLAP_MS) / 1000) * 1000).toISOString();
}

/** An answer read with a cut, joined onto the last answer for the same filter. The answer holds every live run and
 * every run that ended after the cut. The last answer's runs that are not in it and ended at or before the cut are
 * kept, after the answer's rows, where the server's order puts them, and so is a terminal run with no end time,
 * which no cut selects. Any other run the answer leaves out has ended or left the filter, and goes. An answer without
 * a cut is whole and replaces the last. */
export function mergeBoardDelta(last: RunsListFilteredResult, next: RunsListFilteredResult, limit: number): RunsListFilteredResult {
  if (!next.endedAfter) return next;
  const cut = Date.parse(next.endedAfter);
  const fresh = new Set(next.runs.map((r) => r.id));
  const kept = last.runs.filter((r) => {
    if (fresh.has(r.id)) return false;
    const end = runEndTime(r);
    return end ? Date.parse(end) <= cut : isTerminalRunState(r.state);
  });
  const runs = [...next.runs, ...kept];
  return { ...next, runs: runs.slice(0, limit), truncated: next.truncated || last.truncated || runs.length > limit };
}
