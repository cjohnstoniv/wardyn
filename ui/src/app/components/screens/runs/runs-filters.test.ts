/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  DEFAULT_RUNS_FILTERS,
  parseRunsFilters,
  runsFilterToServerStatus,
  runsFiltersAreDefault,
  serializeRunsFilters,
} from "./runs-filters";

// Review F6: DEFAULT_RUNS_FILTERS.endedWithin as a hand-typed literal, not
// read back through the constant — the test right below this one compared
// parseRunsFilters' output against DEFAULT_RUNS_FILTERS itself, a tautology
// that cannot catch the constant changing (the reviewer's M2b mutation, 7d
// -> 30d, survived it).
describe("DEFAULT_RUNS_FILTERS", () => {
  it("H-2: ended runs default to the last 7 days", () => {
    expect(DEFAULT_RUNS_FILTERS.endedWithin).toBe("7d");
  });
});

describe("parseRunsFilters", () => {
  it("defaults an empty URL to the H-2 defaults (7d, killed off)", () => {
    expect(parseRunsFilters(new URLSearchParams())).toEqual({
      q: "",
      status: "all",
      endedWithin: "7d",
      includeKilled: false,
      workspace: "all",
    });
  });

  it("reads every field back", () => {
    const params = new URLSearchParams(
      "q=flaky&status=failed&ended_within=30d&include_killed=1&workspace=acme/widgets",
    );
    expect(parseRunsFilters(params)).toEqual({
      q: "flaky",
      status: "failed",
      endedWithin: "30d",
      includeKilled: true,
      workspace: "acme/widgets",
    });
  });

  it("degrades an invalid status/window to the default rather than crashing", () => {
    const params = new URLSearchParams("status=bogus&ended_within=bogus");
    expect(parseRunsFilters(params).status).toBe("all");
    expect(parseRunsFilters(params).endedWithin).toBe("7d");
  });
});

describe("serializeRunsFilters — round-trips through parseRunsFilters, and a Default view's URL is clean", () => {
  it("the default state serializes to an empty query string", () => {
    expect(serializeRunsFilters(DEFAULT_RUNS_FILTERS).toString()).toBe("");
  });

  it("round-trips a non-default state", () => {
    const state = { q: "x", status: "killed", endedWithin: "24h", includeKilled: true, workspace: "acme/x" } as const;
    const roundTripped = parseRunsFilters(serializeRunsFilters(state));
    expect(roundTripped).toEqual(state);
  });
});

describe("runsFiltersAreDefault", () => {
  it("true only for the exact default", () => {
    expect(runsFiltersAreDefault(DEFAULT_RUNS_FILTERS)).toBe(true);
    expect(runsFiltersAreDefault({ ...DEFAULT_RUNS_FILTERS, q: "x" })).toBe(false);
  });
});

describe("runsFilterToServerStatus", () => {
  it("'all' sends no status param at all", () => {
    expect(runsFilterToServerStatus("all")).toBeUndefined();
  });
  it("every other value passes through", () => {
    expect(runsFilterToServerStatus("needs")).toBe("needs");
    expect(runsFilterToServerStatus("failed")).toBe("failed");
  });
});
