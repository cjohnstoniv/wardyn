/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3041 — the count behind "Filters (n)" and the chips under the phone search.
import { describe, it, expect } from "vitest";
import { activeRunsFilters } from "./runs-active-filters";
import { DEFAULT_RUNS_FILTERS, type RunsFilterState } from "./runs-filters";

const f = (over: Partial<RunsFilterState>): RunsFilterState => ({ ...DEFAULT_RUNS_FILTERS, ...over });

describe("activeRunsFilters", () => {
  it("counts nothing at the defaults, and never counts the search box", () => {
    expect(activeRunsFilters(DEFAULT_RUNS_FILTERS, true)).toEqual([]);
    expect(activeRunsFilters(f({ q: "deploy" }), true)).toEqual([]);
  });

  it("counts one chip per control off its default, labelled as the select reads", () => {
    const a = activeRunsFilters(
      f({ status: "needs", group: "workspace", endedWithin: "30d", workspace: "acme/widgets", includeKilled: true, scope: "mine" }),
      true,
    );
    expect(a.map((x) => x.label)).toEqual([
      "Status · Needs you",
      "Last 30 days",
      "Workspace · acme/widgets",
      "Group · Workspace",
      "Mine",
      "Include killed",
    ]);
  });

  it("the board's example is n = 2: Status and Group", () => {
    expect(activeRunsFilters(f({ status: "needs", group: "workspace" }), false)).toHaveLength(2);
  });

  it("the User view has no Whose runs control, so a stale owner=me adds no chip", () => {
    expect(activeRunsFilters(f({ scope: "mine" }), false)).toEqual([]);
    expect(activeRunsFilters(f({ scope: "mine" }), true)).toHaveLength(1);
  });

  it("removing a chip resets only its control", () => {
    const state = f({ q: "x", status: "failed", group: "title" });
    const [status] = activeRunsFilters(state, true);
    expect(status.key).toBe("status");
    expect(status.without).toEqual({ ...state, status: "all" });
    expect(activeRunsFilters(status.without, true).map((x) => x.key)).toEqual(["group"]);
  });
});
