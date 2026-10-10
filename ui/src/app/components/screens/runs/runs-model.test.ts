/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { AgentRun, RunState } from "../../../lib/types";
import { rowPresentation, sectionRuns, isTopQuiet, nonAttentionRuns } from "./runs-model";
import { glyphKindFor } from "./row-glyph";

// Relative to the moment the suite runs, not a literal date (scripts/
// check-fixture-dates.sh) — every case below offsets from this by a fixed
// duration, so the absolute value never matters.
const NOW = Date.now();

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
  placement: "",
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

describe("rowPresentation — lost (F1, PR #1317 review: Revive-from-row)", () => {
  it("by=you: Sandbox stopped, amber, needsYou, action revive", () => {
    const r = run({ attention: { kind: "lost", by: "you", pending: 0 } });
    expect(rowPresentation(r, false)).toMatchObject({
      hue: "amber",
      word: "Sandbox stopped",
      action: "revive",
      needsYou: true,
    });
  });

  it("by=owner: no action — nobody but the owner can revive it", () => {
    const r = run({ attention: { kind: "lost", by: "owner", pending: 0 } });
    expect(rowPresentation(r, true)).toMatchObject({ action: null, needsYou: false });
  });

  it("in the admin view, a lost run is never by=you (the server forces owner)", () => {
    const r = run({ attention: { kind: "lost", by: "owner", pending: 0 } });
    expect(rowPresentation(r, true).needsYou).toBe(false);
  });

  // F18 (PR #1317 round-2 review): no k8s revive in 0.8 (L6) — the run
  // page's own lifetime banner already treats a k8s lost run as not
  // revivable, so the row must not offer a button the run page won't honour.
  it("by=you on a k8s run: still Needs you, but no action — needsYou survives without a button", () => {
    const r = run({ attention: { kind: "lost", by: "you", pending: 0 }, runner_target: "k8s" });
    expect(rowPresentation(r, false)).toMatchObject({
      hue: "amber",
      word: "Sandbox stopped",
      action: null,
      needsYou: true,
    });
  });
});

describe("rowPresentation — a lease-ended run", () => {
  it("reads grey 'Ended at its end time', never the live blue Running word — even though state is still RUNNING", () => {
    const r = run({ state: "RUNNING", lost_reason: "ended", lost_at: new Date(NOW - 3600_000).toISOString() });
    expect(rowPresentation(r, false)).toMatchObject({
      hue: "grey",
      word: "Ended at its end time",
      action: null,
      needsYou: false,
    });
  });
});

describe("rowPresentation — STARTING/PENDING carry the substrate's stage line", () => {
  it("STARTING with a status_detail gets it as the subword", () => {
    const r = run({ state: "STARTING", status_detail: "image: Pulling: acme/agent:latest", status_reason: "Pulling" });
    expect(rowPresentation(r, false).subword).toBeTruthy();
  });

  it("PENDING/STARTING with no status_detail has no subword (never an empty string)", () => {
    const r = run({ state: "STARTING" });
    expect(rowPresentation(r, false).subword).toBeUndefined();
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

describe("rowPresentation — RL-15: the board's own ends-soon warning (design.md §2.3)", () => {
  it("10 minutes left: amber 'Ends in 10 minutes', no attention needed", () => {
    const r = run({ state: "RUNNING", ends_at: new Date(NOW + 9 * 60_000).toISOString() });
    expect(rowPresentation(r, false)).toMatchObject({
      hue: "amber",
      word: "Ends in 10 minutes",
      action: null,
      needsYou: false,
    });
  });

  it("1 hour left: amber 'Ends in 1 hour'", () => {
    const r = run({ state: "RUNNING", ends_at: new Date(NOW + 45 * 60_000).toISOString() });
    expect(rowPresentation(r, false).word).toBe("Ends in 1 hour");
  });

  it("24 hours left on a lease over 2 days: amber 'Ends in 24 hours'", () => {
    const r = run({
      created_at: new Date(NOW - 3 * 24 * 3600_000).toISOString(),
      state: "RUNNING",
      ends_at: new Date(NOW + 20 * 3600_000).toISOString(),
    });
    expect(rowPresentation(r, false).word).toBe("Ends in 24 hours");
  });

  it("24 hours left on a short (<= 2 day) lease: no warning yet — plain Running", () => {
    const r = run({
      created_at: new Date(NOW - 60_000).toISOString(),
      state: "RUNNING",
      ends_at: new Date(NOW + 20 * 3600_000).toISOString(),
    });
    expect(rowPresentation(r, false).word).toBe("Running");
  });

  it("no end (ends_at absent): plain Running, never a warning", () => {
    const r = run({ state: "RUNNING" });
    expect(rowPresentation(r, false).word).toBe("Running");
  });

  it("a run already past its end (should have been marked ended/lost): no warning, not negative", () => {
    const r = run({ state: "RUNNING", ends_at: new Date(NOW - 60_000).toISOString() });
    expect(rowPresentation(r, false).word).toBe("Running");
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

  it("a lease-ended row (state still RUNNING) gets the square outline, not the pulsing dot", () => {
    // rowPresentation gives it grey + "Ended at its end time" while `state`
    // stays RUNNING (see runs-model.ts's own note) — the RUNNING check must
    // not win the shape decision for this word.
    expect(glyphKindFor("grey", "Ended at its end time", "RUNNING")).toBe("square-outline");
  });
});

describe("sectionRuns — need, then time (H-1/H-6)", () => {
  // Calendar buckets need same-day fixtures even when the suite starts just
  // after midnight. Other suites still use the real clock for lifetime tests.
  const localNoon = new Date(NOW);
  localNoon.setHours(12, 0, 0, 0);
  const sectionNow = localNoon.getTime();
  it("splits decide / running / ended-today / earlier-this-week", () => {
    const decideRun = run({ id: "d1", attention: { kind: "approval", by: "you", pending: 1 } });
    const runningRun = run({ id: "r1", state: "RUNNING" });
    const endedTodayRun = run({
      id: "e1",
      state: "COMPLETED",
      ended_at: new Date(sectionNow - 60_000).toISOString(),
    });
    const earlierRun = run({
      id: "e2",
      state: "COMPLETED",
      ended_at: new Date(sectionNow - 2 * 24 * 3600_000).toISOString(),
    });
    const s = sectionRuns([decideRun, runningRun, endedTodayRun, earlierRun], sectionNow);
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
      ended_at: new Date(sectionNow - 10 * 24 * 3600_000).toISOString(),
    });
    const oldRun = run({
      id: "o1",
      state: "COMPLETED",
      ended_at: new Date(sectionNow - 40 * 24 * 3600_000).toISOString(),
    });
    const s = sectionRuns([monthRun, oldRun], sectionNow);
    expect(s.older).toEqual([
      { label: "Earlier this month", runs: [monthRun] },
      { label: "Older", runs: [oldRun] },
    ]);
  });

  it("a run needing an admin (by!=you) is NOT in decide — it stays in running, amber", () => {
    const r = run({ state: "RUNNING", attention: { kind: "approval", by: "admin", pending: 1 } });
    const s = sectionRuns([r], sectionNow);
    expect(s.decide).toEqual([]);
    expect(s.running.map((x) => x.id)).toEqual(["run-1"]);
  });

  it("by=owner (reauth/ado_consent/lost, Admin view only) lands in waitingOwner, not decide or running", () => {
    const reauth = run({ id: "w1", state: "RUNNING", attention: { kind: "reauth", by: "owner", pending: 1 } });
    const lost = run({ id: "w2", state: "RUNNING", attention: { kind: "lost", by: "owner", pending: 0 } });
    const s = sectionRuns([reauth, lost], sectionNow);
    expect(s.waitingOwner.map((r) => r.id)).toEqual(["w1", "w2"]);
    expect(s.decide).toEqual([]);
    expect(s.running).toEqual([]);
  });

  // a lease-ended run (state still RUNNING, lost_reason "ended")
  // must land in the ENDED buckets, not Running — and its age is keyed on
  // lost_at (the lease end), not ended_at (absent) or updated_at.
  it("a lease-ended run is bucketed as over, keyed on lost_at, never in running", () => {
    const r = run({
      id: "le1",
      state: "RUNNING",
      lost_reason: "ended",
      lost_at: new Date(sectionNow - 60_000).toISOString(),
    });
    const s = sectionRuns([r], sectionNow);
    expect(s.running).toEqual([]);
    expect(s.endedToday.map((x) => x.id)).toEqual(["le1"]);
  });
});

describe("nonAttentionRuns — the flat list Group by Workspace/Title regroups (#1197 L4 H-6)", () => {
  it("excludes decide and waitingOwner runs, keeps everything else in order", () => {
    const decideRun = run({ id: "d1", attention: { kind: "approval", by: "you", pending: 1 } });
    const ownerRun = run({ id: "w1", state: "RUNNING", attention: { kind: "reauth", by: "owner", pending: 1 } });
    const runningRun = run({ id: "r1", state: "RUNNING" });
    const endedRun = run({ id: "e1", state: "COMPLETED", ended_at: new Date(NOW - 60_000).toISOString() });
    const adminWaitRun = run({ state: "RUNNING", id: "a1", attention: { kind: "approval", by: "admin", pending: 1 } });
    expect(nonAttentionRuns([decideRun, ownerRun, runningRun, endedRun, adminWaitRun]).map((r) => r.id)).toEqual([
      "r1",
      "e1",
      "a1",
    ]);
  });
});

describe("isTopQuiet", () => {
  it("true when decide, waitingOwner and running are all empty, regardless of ended sections", () => {
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

  it("false when only an owner's sign-in is waiting (#1197 L4 H-3) — that is not quiet either", () => {
    const s = sectionRuns([run({ state: "RUNNING", attention: { kind: "reauth", by: "owner", pending: 1 } })], NOW);
    expect(isTopQuiet(s)).toBe(false);
  });
});
