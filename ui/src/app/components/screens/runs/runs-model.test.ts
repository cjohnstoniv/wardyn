/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { AgentRun, RunState } from "../../../lib/types";
import { rowPresentation, sectionRuns, isTopQuiet } from "./runs-model";
import { glyphKindFor } from "./row-glyph";

const NOW = Date.parse("2026-09-27T15:00:00.000Z");

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
  id: "run-1",
  created_at: new Date(NOW - 60_000).toISOString(),
  updated_at: new Date(NOW - 60_000).toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "Fix flaky auth tests",
  confinement_class: "CC2",
  state: "RUNNING" as RunState,
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  ...over,
});

describe("rowPresentation — held approval (design.md §2.2 row 1)", () => {
  it("by=you in the user view: Needs your approval, amber, Review, needsYou", () => {
    const r = run({ attention: { kind: "approval", by: "you", pending: 1 } });
    const p = rowPresentation(r, false);
    expect(p).toMatchObject({ hue: "amber", word: "Needs your approval", action: "review", needsYou: true });
    expect(p.subword).toBe("1 waiting · sandbox held");
  });

  it("by=you in the admin view: Needs a decision, still Review", () => {
    const r = run({ attention: { kind: "approval", by: "you", pending: 2 } });
    const p = rowPresentation(r, true);
    expect(p).toMatchObject({ hue: "amber", word: "Needs a decision", action: "review", needsYou: true });
  });

  it("by=admin (H-3): Waiting for an admin, amber, no action, NOT needs-you — in either view", () => {
    const r = run({ attention: { kind: "approval", by: "admin", pending: 1 } });
    expect(rowPresentation(r, false)).toMatchObject({
      hue: "amber",
      word: "Waiting for an admin",
      action: null,
      needsYou: false,
    });
    expect(rowPresentation(r, true)).toMatchObject({
      hue: "amber",
      word: "Waiting for an admin",
      action: null,
      needsYou: false,
    });
  });
});

describe("rowPresentation — reauth / ado_consent (design.md §2.2 rows 2-3)", () => {
  it("by=you (user view): the owner's own AWS sign-in hold, Sign in, needsYou", () => {
    const r = run({ attention: { kind: "reauth", by: "you", pending: 1 } });
    expect(rowPresentation(r, false)).toMatchObject({
      hue: "amber",
      word: "Waiting for your AWS sign-in",
      action: "sign-in",
      needsYou: true,
    });
  });

  it("by=owner (admin view): the OWNER's sign-in, no action, not needs-you", () => {
    const r = run({ attention: { kind: "reauth", by: "owner", pending: 1 } });
    expect(rowPresentation(r, true)).toMatchObject({
      hue: "amber",
      word: "Waiting for the owner's AWS sign-in",
      action: null,
      needsYou: false,
    });
  });

  it("ado_consent mirrors reauth with the Azure DevOps sentence", () => {
    const r = run({ attention: { kind: "ado_consent", by: "you", pending: 1 } });
    expect(rowPresentation(r, false).word).toBe("Waiting for your Azure DevOps sign-in");
  });
});

describe("rowPresentation — lost (H-7 seam: no Revive action built here)", () => {
  it("by=you: Sandbox stopped, amber, needsYou, but NO action (L5 builds Revive)", () => {
    const r = run({ attention: { kind: "lost", by: "you", pending: 0 } });
    expect(rowPresentation(r, false)).toMatchObject({
      hue: "amber",
      word: "Sandbox stopped",
      action: null,
      needsYou: true,
    });
  });

  it("in the admin view, a lost run is never by=you (the server forces owner)", () => {
    const r = run({ attention: { kind: "lost", by: "owner", pending: 0 } });
    expect(rowPresentation(r, true).needsYou).toBe(false);
  });
});

describe("rowPresentation — plain state words with no attention", () => {
  it.each([
    ["RUNNING", "blue", "Running"],
    ["STARTING", "blue", "Starting"],
    ["PENDING", "blue", "Queued"],
    ["COMPLETED", "grey", "Completed"],
    ["FAILED", "red", "Failed"],
    ["KILLED", "grey", "Killed"],
    ["STOPPED", "grey", "Stopped"],
    ["ARCHIVED", "grey", "Archived"],
  ] as const)("%s -> hue %s, word %s", (state, hue, word) => {
    const p = rowPresentation(run({ state }), false);
    expect(p.hue).toBe(hue);
    expect(p.word).toBe(word);
    expect(p.action).toBeNull();
    expect(p.needsYou).toBe(false);
  });
});

describe("glyphKindFor — the shape half of the colour/glyph rule", () => {
  it("every needs-you kind (amber) gets the same 'need' disc, regardless of underlying state", () => {
    expect(glyphKindFor("amber", "Waiting for an admin", "RUNNING")).toBe("need");
  });
  it("Failed is the only 'fail' glyph", () => {
    expect(glyphKindFor("red", "Failed", "FAILED")).toBe("fail");
  });
  it("Running is a filled dot, Starting/Pending a ring", () => {
    expect(glyphKindFor("blue", "Running", "RUNNING")).toBe("dot");
    expect(glyphKindFor("blue", "Starting", "STARTING")).toBe("ring");
    expect(glyphKindFor("blue", "Queued", "PENDING")).toBe("ring");
  });
  it("Completed is a check, Killed a filled square, everything else grey an outline", () => {
    expect(glyphKindFor("grey", "Completed", "COMPLETED")).toBe("check");
    expect(glyphKindFor("grey", "Killed", "KILLED")).toBe("square-fill");
    expect(glyphKindFor("grey", "Stopped", "STOPPED")).toBe("square-outline");
    expect(glyphKindFor("grey", "Archived", "ARCHIVED")).toBe("square-outline");
  });
});

describe("sectionRuns — need, then time (H-1/H-6)", () => {
  it("splits decide / running / ended-today / earlier-this-week", () => {
    const decideRun = run({ id: "d1", attention: { kind: "approval", by: "you", pending: 1 } });
    const runningRun = run({ id: "r1", state: "RUNNING" });
    const endedTodayRun = run({
      id: "e1",
      state: "COMPLETED",
      ended_at: new Date(NOW - 60_000).toISOString(),
    });
    const earlierRun = run({
      id: "e2",
      state: "COMPLETED",
      ended_at: new Date(NOW - 2 * 24 * 3600_000).toISOString(),
    });
    const s = sectionRuns([decideRun, runningRun, endedTodayRun, earlierRun], NOW);
    expect(s.decide.map((r) => r.id)).toEqual(["d1"]);
    expect(s.running.map((r) => r.id)).toEqual(["r1"]);
    expect(s.endedToday.map((r) => r.id)).toEqual(["e1"]);
    expect(s.earlier.map((r) => r.id)).toEqual(["e2"]);
    expect(s.older).toEqual([]);
  });

  it("buckets anything past 7 days into 'Earlier this month' / 'Older' (only reached once a filter widens the window)", () => {
    const monthRun = run({
      id: "m1",
      state: "COMPLETED",
      ended_at: new Date(NOW - 10 * 24 * 3600_000).toISOString(),
    });
    const oldRun = run({
      id: "o1",
      state: "COMPLETED",
      ended_at: new Date(NOW - 40 * 24 * 3600_000).toISOString(),
    });
    const s = sectionRuns([monthRun, oldRun], NOW);
    expect(s.older).toEqual([
      { label: "Earlier this month", runs: [monthRun] },
      { label: "Older", runs: [oldRun] },
    ]);
  });

  it("a run needing an admin (by!=you) is NOT in decide — it stays in running, amber", () => {
    const r = run({ state: "RUNNING", attention: { kind: "approval", by: "admin", pending: 1 } });
    const s = sectionRuns([r], NOW);
    expect(s.decide).toEqual([]);
    expect(s.running.map((x) => x.id)).toEqual(["run-1"]);
  });
});

describe("isTopQuiet", () => {
  it("true when decide and running are both empty, regardless of ended sections", () => {
    const s = sectionRuns(
      [run({ id: "e1", state: "COMPLETED", ended_at: new Date(NOW - 60_000).toISOString() })],
      NOW,
    );
    expect(isTopQuiet(s)).toBe(true);
  });

  it("false once anything needs you or is running", () => {
    const s = sectionRuns([run({ id: "r1", state: "RUNNING" })], NOW);
    expect(isTopQuiet(s)).toBe(false);
  });
});
