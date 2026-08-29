/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { AgentRun, ApprovalRequest, RunState } from "../../../lib/types";
import {
  INTERACTIVE_HEADLINE,
  NO_REPO,
  approvalSignals,
  needsAttention,
  needsYou,
  repoLabel,
  rowHeadline,
  titleGroups,
} from "./board-groups";

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
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

const approval = (over: Partial<ApprovalRequest> = {}): ApprovalRequest => ({
  id: "a1",
  run_id: "run-1",
  kind: "egress_domain",
  requested_scope: { host: "unlisted.example" },
  state: "PENDING",
  requested_at: new Date().toISOString(),
  ...over,
});

// The join that makes "needs you" mean something the run state cannot say: a
// wait_for_review approval parks the sandbox while the run stays RUNNING.
describe("approvalSignals — held vs passive", () => {
  it("marks a wait_for_review approval as held, and counts what is waiting", () => {
    const s = approvalSignals([
      approval({ requested_scope: { host: "held.example", mode: "wait_for_review" } }),
    ]);
    expect(s.get("run-1")).toEqual({ pending: 1, held: true });
  });

  it("marks a plain deny_with_review pending as passive, never held", () => {
    const s = approvalSignals([approval()]);
    expect(s.get("run-1")).toEqual({ pending: 1, passiveHold: true });
  });

  it("a held sibling wins: one parked request holds the run however many passive ones there are", () => {
    const s = approvalSignals([
      approval({ id: "a1" }),
      approval({ id: "a2", requested_scope: { host: "h", mode: "wait_for_review" } }),
      approval({ id: "a3" }),
    ]);
    expect(s.get("run-1")).toMatchObject({ held: true, pending: 3 });
  });

  it("keys by run, and ignores anything already decided", () => {
    const s = approvalSignals([
      approval({ id: "a1", run_id: "run-1" }),
      approval({ id: "a2", run_id: "run-2" }),
      approval({ id: "a3", run_id: "run-3", state: "APPROVED" }),
    ]);
    expect([...s.keys()].sort()).toEqual(["run-1", "run-2"]);
  });

  it("a wait_for_review request whose 30s hold has elapsed is pending, not held — the sandbox is no longer parked", () => {
    const stale = new Date(Date.now() - 60_000).toISOString();
    const s = approvalSignals([
      approval({ requested_scope: { host: "h", mode: "wait_for_review" }, requested_at: stale }),
    ]);
    expect(s.get("run-1")).toEqual({ pending: 1, passiveHold: true });
  });
});

describe("needsAttention / needsYou", () => {
  const none = new Map();

  it("a held approval needs you even though the run state says RUNNING", () => {
    const signals = approvalSignals([
      approval({ requested_scope: { host: "h", mode: "wait_for_review" } }),
    ]);
    const r = run({ state: "RUNNING" });
    expect(needsAttention(r, signals)).toBe(true);
    expect(needsYou(r, signals)).toBe(true);
    // …and the same run with nothing parked on it does not.
    expect(needsAttention(r, none)).toBe(false);
    expect(needsYou(r, none)).toBe(false);
  });

  it("WAITING_FOR_CONFIRMATION needs you with no approval row at all", () => {
    const r = run({ state: "WAITING_FOR_CONFIRMATION" });
    expect(needsAttention(r, none)).toBe(true);
    expect(needsYou(r, none)).toBe(true);
  });

  // An approval is a REQUEST; a failure is a REPORT. Both want eyes, only the
  // request gets pinned — see the lane's own test.
  it("FAILED and KILLED need attention but are NOT pinned to the lane", () => {
    for (const state of ["FAILED", "KILLED"] as const) {
      const r = run({ state });
      expect(needsAttention(r, none)).toBe(true);
      expect(needsYou(r, none)).toBe(false);
    }
  });

  it("an ordinary run in any quiet state needs neither", () => {
    for (const state of ["PENDING", "STARTING", "RUNNING", "COMPLETED", "STOPPED"] as const) {
      expect(needsAttention(run({ state }), none)).toBe(false);
    }
  });
});

describe("rowHeadline — honest fallbacks, nothing invented", () => {
  it("names itself by title outside a group, and by task inside one", () => {
    const r = run({ title: "Nightly audit", task: "Check the lockfiles" });
    expect(rowHeadline(r, false)).toBe("Nightly audit");
    expect(rowHeadline(r, true)).toBe("Check the lockfiles");
  });

  it("falls back to the workspace it mounted when it has no name of its own", () => {
    const r = run({ title: "", task: "", workspace_path: "/home/ops/work/payments-api/" });
    expect(rowHeadline(r, false)).toBe("payments-api");
  });

  it("an interactive run with nothing at all is named by its mode", () => {
    const r = run({ title: "", task: "", workspace_path: "", interactive: true });
    expect(rowHeadline(r, false)).toBe(INTERACTIVE_HEADLINE);
    expect(rowHeadline(r, true)).toBe(INTERACTIVE_HEADLINE);
  });

  it("never claims a session for a run that is not one — a nameless batch run keeps the dash", () => {
    const r = run({ title: "", task: "", workspace_path: "", interactive: false });
    expect(rowHeadline(r, false)).toBe("—");
  });
});

describe("repoLabel", () => {
  it("prefers the repo, falls back to the workspace path, and types both as literals", () => {
    expect(repoLabel(run({ repo: "acme/widgets" }))).toEqual({ text: "acme/widgets", mono: true });
    expect(repoLabel(run({ repo: "", workspace_path: "/srv/scratch" }))).toEqual({
      text: "/srv/scratch",
      mono: true,
    });
  });

  it("says what an ephemeral run actually is rather than rendering a blank — as a phrase, not mono", () => {
    expect(repoLabel(run({ repo: "", workspace_path: "" }))).toEqual({ text: NO_REPO, mono: false });
  });
});

describe("titleGroups", () => {
  it("groups a shared title, leaves singletons and untitled runs loose, and preserves input order", () => {
    const { groups, loose } = titleGroups([
      run({ id: "a", title: "Nightly audit" }),
      run({ id: "b", title: "One-off" }),
      run({ id: "c", title: "Nightly audit" }),
      run({ id: "d", title: "" }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].runs.map((r) => r.id)).toEqual(["a", "c"]);
    expect(loose.map((r) => r.id)).toEqual(["b", "d"]);
  });
});
