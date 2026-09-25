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
  groupWaitBreakdown,
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

  // A tool_call carries {tool,cmd,env} and NO mode, while wardyn-toolgate blocks
  // the agent on the PENDING row itself — so the row is the hold, and reading it
  // as passive left the run out of the Needs-you lane with the sandbox parked.
  it("marks a tool_call approval as held even though its scope carries no mode", () => {
    const s = approvalSignals([
      approval({ kind: "tool_call", requested_scope: { tool: "Bash", cmd: "rm -rf build" } }),
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

  // S10 round 2 (F13): an Azure DevOps consent row is credential_reauth too,
  // but a DIFFERENT provider — it must set adoConsent, never reauth, so the
  // board chip never says "AWS" for it.
  it("marks an Azure DevOps consent row as adoConsent, not reauth", () => {
    const s = approvalSignals([
      approval({
        kind: "credential_reauth",
        requested_scope: { lane: "azure_devops", mechanism: "entra_consent", owner: "dana", provider_id: "row_1", scopes: [] },
      }),
    ]);
    expect(s.get("run-1")?.adoConsent).toBe(true);
    expect(s.get("run-1")?.reauth).toBeUndefined();
  });

  it("still marks a plain AWS credential_reauth row as reauth, not adoConsent", () => {
    const s = approvalSignals([
      approval({ kind: "credential_reauth", requested_scope: { mechanism: "bedrock_sso", owner: "dana" } }),
    ]);
    expect(s.get("run-1")?.reauth).toBe(true);
    expect(s.get("run-1")?.adoConsent).toBeUndefined();
  });

  // #509 — a PENDING tool_call/credential_reauth row is live until the
  // SERVER says otherwise; approvalSignals only ever joins PENDING rows (the
  // `state !== "PENDING"` guard above), so there is no client elapsed-time
  // ceiling left to cross here — both stay held at any age.
  it("a tool_call 2 hours old — well past the old 60-minute ceiling — is still held, not staleHeld", () => {
    const old = new Date(Date.now() - 2 * 60 * 60_000).toISOString();
    const s = approvalSignals([
      approval({ kind: "tool_call", requested_scope: { tool: "Bash", cmd: "rm -rf build" }, requested_at: old }),
    ]);
    expect(s.get("run-1")).toEqual({ pending: 1, held: true });
  });

  it("a credential_reauth 25 hours old — past the server's own 24h default — is still held AND reauth", () => {
    const old = new Date(Date.now() - 25 * 60 * 60_000).toISOString();
    const s = approvalSignals([
      approval({ kind: "credential_reauth", requested_scope: {}, requested_at: old }),
    ]);
    expect(s.get("run-1")).toEqual({ pending: 1, held: true, reauth: true });
  });

  it("a credential_reauth well inside the ceiling is both held AND reauth", () => {
    const s = approvalSignals([approval({ kind: "credential_reauth", requested_scope: {} })]);
    expect(s.get("run-1")).toEqual({ pending: 1, held: true, reauth: true });
  });

  // #725/F1 — an Azure DevOps capability escalation (tool_call with grant_id +
  // requested_scope.lane "azure_devops") releases its OWN hold after 4
  // minutes (isHeld's ADO_HOLD_WINDOW_MS arm), while a plain tool_call stays
  // held for as long as it is PENDING (#509). At 5 minutes it must be a passive
  // pending — never held — so the board card
  // renders RUN_WAIT.waiting(n) rather than waitingHeld(n), matching what
  // the ADO card itself already says at that age.
  it("an Azure DevOps capability tool_call at 5 minutes is a passive pending, not held", () => {
    const fiveMinutesAgo = new Date(Date.now() - 5 * 60_000).toISOString();
    const s = approvalSignals([
      approval({
        kind: "tool_call",
        grant_id: "grant-1",
        requested_scope: { lane: "azure_devops", provider_id: "p1", org: "o1", grant_id: "grant-1", capability: "pr", repo: "r1", tool: "t", cmd: "c" },
        requested_at: fiveMinutesAgo,
      }),
    ]);
    expect(s.get("run-1")).toEqual({ pending: 1, passiveHold: true });
  });

  it("an Azure DevOps capability tool_call still inside its own 4-minute window is held", () => {
    const s = approvalSignals([
      approval({
        kind: "tool_call",
        grant_id: "grant-1",
        requested_scope: { lane: "azure_devops", provider_id: "p1", org: "o1", grant_id: "grant-1", capability: "pr", repo: "r1", tool: "t", cmd: "c" },
      }),
    ]);
    expect(s.get("run-1")).toEqual({ pending: 1, held: true });
  });

  // A row the server has actually moved off PENDING (decided or expired) is
  // filtered before isHeld even runs — it produces no signal at
  // all, the same as any other decided approval.
  it("an EXPIRED tool_call produces no signal for its run", () => {
    const s = approvalSignals([approval({ kind: "tool_call", state: "EXPIRED" })]);
    expect(s.get("run-1")).toBeUndefined();
  });
});

// #160 — the group header's second chip row is built from this pure count,
// exclusive per run so no run is ever double-counted across reasons.
describe("groupWaitBreakdown", () => {
  it("the five-run acceptance shape: two held, one reauth, one starting, one clean", () => {
    const signals = approvalSignals([
      approval({ id: "a1", run_id: "r1", kind: "tool_call", requested_scope: { tool: "Bash", cmd: "ls" } }),
      approval({ id: "a2", run_id: "r2", kind: "tool_call", requested_scope: { tool: "Bash", cmd: "rm -rf x" } }),
      approval({ id: "a3", run_id: "r3", kind: "credential_reauth", requested_scope: {} }),
    ]);
    const runs = [
      run({ id: "r1", state: "WAITING_FOR_CONFIRMATION" }),
      run({ id: "r2", state: "WAITING_FOR_CONFIRMATION" }),
      run({ id: "r3", state: "RUNNING" }),
      run({ id: "r4", state: "STARTING" }),
      run({ id: "r5", state: "RUNNING" }), // clean — counted nowhere
    ];
    expect(groupWaitBreakdown(runs, signals)).toEqual({ held: 2, reauth: 1, starting: 1 });
  });

  // #509 — a PENDING tool_call stays held at any age (no client ceiling), so
  // a group's counted claim no longer shrinks as its holds age.
  it("a tool_call held for hours still counts as held in the header's breakdown", () => {
    const old = new Date(Date.now() - 2 * 60 * 60_000).toISOString();
    const signals = approvalSignals([
      approval({ id: "a1", run_id: "r1", kind: "tool_call", requested_scope: { tool: "Bash", cmd: "ls" } }),
      approval({ id: "a2", run_id: "r2", kind: "tool_call", requested_scope: { tool: "Bash", cmd: "ls" }, requested_at: old }),
    ]);
    const runs = [
      run({ id: "r1", state: "WAITING_FOR_CONFIRMATION" }),
      run({ id: "r2", state: "WAITING_FOR_CONFIRMATION" }),
    ];
    expect(groupWaitBreakdown(runs, signals)).toEqual({ held: 2, reauth: 0, starting: 0 });
  });

  it("no signals and no STARTING runs counts nothing — the caller renders the uncounted 'Nothing waiting' chip itself", () => {
    const runs = [run({ id: "r1", state: "RUNNING" }), run({ id: "r2", state: "COMPLETED" })];
    expect(groupWaitBreakdown(runs, new Map())).toEqual({ held: 0, reauth: 0, starting: 0 });
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

  it("a pending tool_call pins a RUNNING run too — the toolgate is holding the agent", () => {
    const signals = approvalSignals([
      approval({ kind: "tool_call", requested_scope: { tool: "Bash", cmd: "rm -rf build" } }),
    ]);
    const r = run({ state: "RUNNING" });
    expect(needsAttention(r, signals)).toBe(true);
    expect(needsYou(r, signals)).toBe(true);
  });

  it("monitoring counts for needsAttention but NOT for needsYou — a passive pending is not a request", () => {
    const signals = approvalSignals([approval()]);
    const r = run({ state: "RUNNING" });
    expect(needsAttention(r, signals)).toBe(true);
    expect(needsYou(r, signals)).toBe(false);
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
