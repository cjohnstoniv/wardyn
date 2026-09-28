/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  applySavedViewOwner,
  DEFAULT_RUNS_FILTERS,
  parseRunsFilters,
  runsFilterToServerOwner,
  runsFilterToServerStatus,
  runsFiltersAreDefault,
  serializeRunsFilters,
} from "./runs-filters";

// DEFAULT_RUNS_FILTERS.endedWithin as a hand-typed literal, not
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
  it("defaults an empty URL to the H-2/H-4/H-6 defaults (7d, killed off, Everyone, Sections)", () => {
    // Hand-typed, not read back through DEFAULT_RUNS_FILTERS itself —
    // that would be a tautology unable to catch the constant drifting
    // (a mutated default, 7d -> 30d, would survive a self-comparison).
    expect(parseRunsFilters(new URLSearchParams())).toEqual({
      q: "",
      status: "all",
      endedWithin: "7d",
      includeKilled: false,
      workspace: "all",
      scope: "all",
      group: "sections",
    });
  });

  it("reads every field back", () => {
    const params = new URLSearchParams(
      "q=flaky&status=failed&ended_within=30d&include_killed=1&workspace=acme/widgets&owner=me&group=title",
    );
    expect(parseRunsFilters(params)).toEqual({
      q: "flaky",
      status: "failed",
      endedWithin: "30d",
      includeKilled: true,
      workspace: "acme/widgets",
      scope: "mine",
      group: "title",
    });
  });

  it("degrades an invalid status/window/group to the default rather than crashing", () => {
    const params = new URLSearchParams("status=bogus&ended_within=bogus&group=bogus");
    expect(parseRunsFilters(params).status).toBe("all");
    expect(parseRunsFilters(params).endedWithin).toBe("7d");
    expect(parseRunsFilters(params).group).toBe("sections");
  });

  it("any owner value other than 'me' reads as Everyone (H-4's fail-open default)", () => {
    expect(parseRunsFilters(new URLSearchParams("owner=all")).scope).toBe("all");
    expect(parseRunsFilters(new URLSearchParams("owner=bogus")).scope).toBe("all");
  });
});

describe("serializeRunsFilters — round-trips through parseRunsFilters, and a Default view's URL is clean", () => {
  it("the default state serializes to an empty query string", () => {
    expect(serializeRunsFilters(DEFAULT_RUNS_FILTERS).toString()).toBe("");
  });

  it("round-trips a non-default state", () => {
    const state = {
      q: "x",
      status: "killed",
      endedWithin: "24h",
      includeKilled: true,
      workspace: "acme/x",
      scope: "mine",
      group: "workspace",
    } as const;
    const roundTripped = parseRunsFilters(serializeRunsFilters(state));
    expect(roundTripped).toEqual(state);
  });

  it("Mine serializes to owner=me, the same param name the server filter reads", () => {
    const params = serializeRunsFilters({ ...DEFAULT_RUNS_FILTERS, scope: "mine" });
    expect(params.get("owner")).toBe("me");
  });

  it("a non-default group serializes; the default (sections) does not", () => {
    expect(serializeRunsFilters({ ...DEFAULT_RUNS_FILTERS, group: "workspace" }).get("group")).toBe("workspace");
    expect(serializeRunsFilters(DEFAULT_RUNS_FILTERS).has("group")).toBe(false);
  });
});

describe("runsFiltersAreDefault", () => {
  it("true only for the exact default", () => {
    expect(runsFiltersAreDefault(DEFAULT_RUNS_FILTERS)).toBe(true);
    expect(runsFiltersAreDefault({ ...DEFAULT_RUNS_FILTERS, q: "x" })).toBe(false);
  });

  it("Mine counts as a non-default filter (it narrows the server read)", () => {
    expect(runsFiltersAreDefault({ ...DEFAULT_RUNS_FILTERS, scope: "mine" })).toBe(false);
  });

  it("a non-default group does NOT count — it never changes what was fetched", () => {
    expect(runsFiltersAreDefault({ ...DEFAULT_RUNS_FILTERS, group: "title" })).toBe(true);
  });
});

describe("runsFilterToServerOwner", () => {
  it("Mine sends owner=me", () => {
    expect(runsFilterToServerOwner("mine")).toBe("me");
  });
  it("Everyone sends nothing — never speculatively 'all'", () => {
    expect(runsFilterToServerOwner("all")).toBeUndefined();
  });
});

describe("applySavedViewOwner — Everyone/Mine survives picking a saved view (H-4)", () => {
  it("Admin view, Mine: a built-in view with no owner param gets one added", () => {
    expect(applySavedViewOwner("status=failed", true, "mine")).toBe("status=failed&owner=me");
  });

  it("Admin view, Mine: a custom view saved under Everyone still comes back as Mine", () => {
    expect(applySavedViewOwner("status=failed&owner=me", true, "all")).toBe("status=failed");
  });

  it("Admin view, Everyone: owner is dropped even if the view's own search carried one", () => {
    expect(applySavedViewOwner("owner=me", true, "all")).toBe("");
  });

  it("User view: owner is always dropped, whatever the current scope reads", () => {
    expect(applySavedViewOwner("status=failed", false, "mine")).toBe("status=failed");
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
