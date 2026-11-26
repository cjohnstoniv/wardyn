/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunsListFilteredResult } from "../../../lib/api/runs";
import type { AgentRun } from "../../../lib/types";
import { boardCut, DELTA_OVERLAP_MS, mergeBoardDelta, runEndTime } from "./board-delta";

const T0 = Math.floor(Date.now() / 1000) * 1000;
const at = (secondsBefore: number) => new Date(T0 - secondsBefore * 1000).toISOString();
const live = (id: string): AgentRun => ({ id, state: "RUNNING" }) as AgentRun;
const ended = (id: string, secondsBefore: number): AgentRun =>
  ({ id, state: "COMPLETED", ended_at: at(secondsBefore) }) as AgentRun;
const answer = (runs: AgentRun[], over: Partial<RunsListFilteredResult> = {}): RunsListFilteredResult => ({
  runs,
  truncated: false,
  hiddenOlder: 0,
  hiddenKilled: 0,
  countsRead: false,
  ...over,
});
const ids = (r: RunsListFilteredResult) => r.runs.map((x) => x.id);

describe("runEndTime", () => {
  it("gives a lease-ended run's lost_at even when it has ended_at, otherwise ended_at, and undefined for a live run", () => {
    const lease = { id: "l", state: "RUNNING", lost_reason: "ended", lost_at: at(30), ended_at: at(900) } as AgentRun;
    expect(runEndTime(lease)).toBe(at(30));
    expect(runEndTime(ended("e", 60))).toBe(at(60));
    expect(runEndTime(live("r"))).toBeUndefined();
  });
});

describe("boardCut", () => {
  it("gives the newest end less 120 s, floored to whole seconds, as ISO", () => {
    const newest = new Date(T0 - 5000 + 400).toISOString(); // 11:59:55.400
    const last = answer([live("a"), ended("b", 600), { id: "c", state: "COMPLETED", ended_at: newest } as AgentRun]);
    const cut = boardCut(last);
    expect(cut).toBe(new Date(Math.floor((Date.parse(newest) - DELTA_OVERLAP_MS) / 1000) * 1000).toISOString());
    expect(cut).toBe(new Date(T0 - 5000 - DELTA_OVERLAP_MS).toISOString());
  });

  it("is undefined with no ended run, and for the live rows of status=needs", () => {
    expect(boardCut(answer([]))).toBeUndefined();
    expect(boardCut(answer([live("a"), live("b")]))).toBeUndefined();
  });
});

describe("mergeBoardDelta", () => {
  const cutAt = at(120);

  it("returns an answer without endedAfter as it is", () => {
    const next = answer([live("a")]);
    expect(mergeBoardDelta(answer([ended("old", 900)]), next, 1000)).toBe(next);
  });

  it("keeps the last answer's runs that ended at or before the cut, after the answer's rows, in their old order", () => {
    const last = answer([live("l1"), ended("e1", 200), ended("e2", 900), ended("e3", 120)]);
    const next = answer([live("l1"), ended("n1", 10)], { endedAfter: cutAt });
    expect(ids(mergeBoardDelta(last, next, 1000))).toEqual(["l1", "n1", "e1", "e2", "e3"]);
  });

  it("drops a last-answer live run the answer no longer holds", () => {
    const last = answer([live("gone"), ended("e1", 900)]);
    const next = answer([], { endedAfter: cutAt });
    expect(ids(mergeBoardDelta(last, next, 1000))).toEqual(["e1"]);
  });

  it("drops a last-answer run that ended after the cut and is absent", () => {
    const last = answer([ended("recent", 30), ended("e1", 900)]);
    const next = answer([], { endedAfter: cutAt });
    expect(ids(mergeBoardDelta(last, next, 1000))).toEqual(["e1"]);
  });

  it("takes the answer's copy on a shared id", () => {
    const last = answer([{ ...ended("x", 900), title: "old" } as AgentRun]);
    const next = answer([{ ...ended("x", 900), title: "new" } as AgentRun], { endedAfter: cutAt });
    const out = mergeBoardDelta(last, next, 1000);
    expect(out.runs).toHaveLength(1);
    expect(out.runs[0].title).toBe("new");
  });

  it("keeps a COMPLETED run with no ended_at", () => {
    const legacy = { id: "legacy", state: "COMPLETED" } as AgentRun;
    const out = mergeBoardDelta(answer([legacy]), answer([live("a")], { endedAfter: cutAt }), 1000);
    expect(ids(out)).toEqual(["a", "legacy"]);
  });

  it("cuts to the limit and marks the answer truncated", () => {
    const next = answer(Array.from({ length: 990 }, (_, i) => live(`n${i}`)), { endedAfter: cutAt });
    const last = answer(Array.from({ length: 20 }, (_, i) => ended(`k${i}`, 900 + i)));
    const out = mergeBoardDelta(last, next, 1000);
    expect(out.runs).toHaveLength(1000);
    expect(out.truncated).toBe(true);
  });

  it("carries the last answer's truncated flag", () => {
    const out = mergeBoardDelta(answer([ended("e", 900)], { truncated: true }), answer([], { endedAfter: cutAt }), 1000);
    expect(out.truncated).toBe(true);
  });
});
