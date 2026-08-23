/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Ported from the retired import-workspace/import-types.test.ts — only the
// "record helpers" coverage survives (the rail/verify-checklist describe
// blocks tested helpers that no longer exist; see session-helpers.ts's header
// comment for what was dropped and why). Adds coverage for sessionStage and
// verifyKeyOf/policyNameFor, new/renamed since the per-session merge.
import { describe, it, expect } from "vitest";
import {
  recordSessions,
  orphanedVerifySessions,
  sessionKeyOf,
  recordResult,
  isRecording,
  newEgressHosts,
  egressPromotionDiff,
  isEmptyCapture,
  lastCleanReplay,
  sessionStage,
  verifyKeyOf,
  policyNameFor,
} from "./session-helpers";
import type { RecordResult, Workspace } from "../../../lib/types";

const ws = (over: Partial<Workspace> = {}): Workspace => ({
  id: "w",
  name: "n",
  kind: "repo",
  source: "s",
  status: "scanned",
  created_at: "",
  updated_at: "",
  ...over,
});

describe("record helpers — read the server-authored record fields", () => {
  it("recordSessions lists the OPEN record_results (key + label), ordered by start time", () => {
    expect(recordSessions(ws())).toEqual([]);
    const w = ws({
      record_results: {
        "agent-loop": { run_id: "r2", label: "agent loop", mode: "interactive", status: "recorded", started_at: "2026-01-02" },
        "build-test": { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded", started_at: "2026-01-01" },
        "no-label": { run_id: "r3", mode: "interactive", status: "recorded", started_at: "2026-01-03" },
        // A confined replay result must never show up as its own "session".
        "verify:build-test": { run_id: "r4", label: "build & test", mode: "interactive", confined: true, status: "recorded" },
      },
    });
    expect(recordSessions(w)).toEqual([
      { key: "build-test", label: "build & test" }, // earliest start first
      { key: "agent-loop", label: "agent loop" },
      { key: "no-label", label: "no-label" }, // falls back to the key
    ]);
  });

  it("sessionKeyOf mirrors the server slug", () => {
    expect(sessionKeyOf("build & test")).toBe("build-test");
    expect(sessionKeyOf("  Agent Dev Loop ")).toBe("agent-dev-loop");
    expect(sessionKeyOf("deploy/dry-run")).toBe("deploy-dry-run");
    expect(sessionKeyOf("***")).toBe("");
  });

  it("verifyKeyOf prefixes the confined-replay key; policyNameFor slugs workspace+recording", () => {
    expect(verifyKeyOf("build-test")).toBe("verify:build-test");
    expect(policyNameFor("payments", "build & test")).toBe("payments-build-test");
  });

  it("orphanedVerifySessions surfaces a confined result with no open sibling (the wizard's verify:verify)", () => {
    expect(orphanedVerifySessions(ws())).toEqual([]);
    const w = ws({
      record_results: {
        // The wizard's live Verify session: stored confined under
        // "verify:verify" with no OPEN "verify" entry to be a replay of.
        "verify:verify": { run_id: "r1", label: "verify", mode: "interactive", confined: true, status: "recording" },
        // A NORMAL session's confined replay — its open sibling "build-test"
        // exists, so this must NOT be treated as orphaned.
        "build-test": { run_id: "r2", label: "build & test", mode: "interactive", status: "recorded" },
        "verify:build-test": { run_id: "r3", label: "build & test", mode: "interactive", confined: true, status: "recorded" },
      },
    });
    expect(orphanedVerifySessions(w)).toEqual([{ key: "verify:verify", label: "verify" }]);
    // And recordSessions (open-only) never lists either confined key.
    expect(recordSessions(w)).toEqual([{ key: "build-test", label: "build & test" }]);
  });

  it("recordResult reads straight off the workspace (never derive)", () => {
    const rr: RecordResult = { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded" };
    const w = ws({ record_results: { "build-test": rr } });
    expect(recordResult(w, "build-test")).toBe(rr);
    expect(recordResult(w, "missing")).toBeUndefined();
  });

  it("isRecording is true iff any session's run (open or confined replay) is still in flight", () => {
    expect(isRecording(ws())).toBe(false);
    expect(isRecording(ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "recorded" } } }))).toBe(false);
    expect(
      isRecording(ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "recording" } } })),
    ).toBe(true);
    expect(
      isRecording(
        ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "recorded" }, "verify:s": { run_id: "r2", mode: "interactive", confined: true, status: "recording" } } }),
      ),
    ).toBe(true);
  });

  it("newEgressHosts = observed (allow_count>0) minus approved minus profile egress, dedup", () => {
    const rr: RecordResult = {
      run_id: "r1",
      mode: "auto",
      status: "recorded",
      observations: {
        domains: [
          { host: "registry.npmjs.org", allow_count: 3, deny_count: 0, pending_count: 0 },
          { host: "github.com", allow_count: 1, deny_count: 0, pending_count: 0 }, // already approved
          { host: "cdn.jsdelivr.net", allow_count: 2, deny_count: 0, pending_count: 0 }, // profile auto-allowed
          { host: "blocked.example", allow_count: 0, deny_count: 5, pending_count: 0 }, // never reached
        ],
        minted_grant_ids: [],
        exec_argv0s: [],
        file_writes: [],
        connects: [],
        anomalies: [],
      },
    };
    const w = ws({
      record_results: { code: rr },
      approved_egress: ["github.com"],
      profile: { egress_domains: ["cdn.jsdelivr.net"] } as unknown as Record<string, unknown>,
    });
    expect(newEgressHosts(w, "code")).toEqual(["registry.npmjs.org"]);
  });

  // W20-S1-1: a host already covered by an effective egress:required
  // requirement row (e.g. approved earlier via the workspace wizard, never
  // written to the legacy approved_egress/profile lanes) must NOT be offered
  // again — the server's own dedup (handlePromoteRecordEgress) drops it, so
  // offering it here promotes zero new rows while the card still claims
  // "Promoted".
  it("newEgressHosts also excludes a host already required via effective_requirements (server dedup parity)", () => {
    const rr: RecordResult = {
      run_id: "r1",
      mode: "auto",
      status: "recorded",
      observations: {
        domains: [
          { host: "registry.npmjs.org", allow_count: 3, deny_count: 0, pending_count: 0 },
          { host: "api.example.com", allow_count: 1, deny_count: 0, pending_count: 0 }, // effective-required
        ],
        minted_grant_ids: [],
        exec_argv0s: [],
        file_writes: [],
        connects: [],
        anomalies: [],
      },
    };
    const w = ws({
      record_results: { code: rr },
      effective_requirements: {
        "egress:api.example.com": { level: "required", provenance: "operator_set" },
      },
    });
    expect(newEgressHosts(w, "code")).toEqual(["registry.npmjs.org"]);
  });

  // W20-S1-1: a platform-plumbing host (the model-provider harness host every
  // session needs, or the console's own origin) must land in its OWN bucket
  // — never "approvable" (nothing to click Approve for) and never
  // "alreadyApproved" (that claims an operator decision that never
  // happened). One function computes all three so a host can't drift
  // between buckets across two separately-derived lists.
  describe("egressPromotionDiff — plumbing hosts get their own bucket, never approvable or falsely 'already approved'", () => {
    it("a model-provider host (api.anthropic.com, or any *.anthropic.com) is plumbing, not approvable", () => {
      const rr: RecordResult = {
        run_id: "r1",
        mode: "auto",
        status: "recorded",
        observations: {
          domains: [
            { host: "api.anthropic.com", allow_count: 5, deny_count: 0, pending_count: 0 },
            { host: "eu.anthropic.com", allow_count: 1, deny_count: 0, pending_count: 0 },
            { host: "api.openai.com", allow_count: 1, deny_count: 0, pending_count: 0 },
            { host: "registry.npmjs.org", allow_count: 3, deny_count: 0, pending_count: 0 },
          ],
          minted_grant_ids: [],
          exec_argv0s: [],
          file_writes: [],
          connects: [],
          anomalies: [],
        },
      };
      const w = ws({ record_results: { code: rr } });
      const diff = egressPromotionDiff(w, "code");
      expect(diff.approvable).toEqual(["registry.npmjs.org"]);
      expect(diff.plumbing.sort()).toEqual(["api.anthropic.com", "api.openai.com", "eu.anthropic.com"]);
      expect(diff.alreadyApproved).toEqual([]);
      // newEgressHosts (the confirm-dialog's own list) is exactly the
      // approvable bucket — never widens past it.
      expect(newEgressHosts(w, "code")).toEqual(diff.approvable);
    });

    it("selfHost (the console's own origin) is plumbing when passed, ignored when omitted", () => {
      const rr: RecordResult = {
        run_id: "r1",
        mode: "auto",
        status: "recorded",
        observations: {
          domains: [{ host: "wardyn.example.com", allow_count: 1, deny_count: 0, pending_count: 0 }],
          minted_grant_ids: [],
          exec_argv0s: [],
          file_writes: [],
          connects: [],
          anomalies: [],
        },
      };
      const w = ws({ record_results: { code: rr } });
      expect(egressPromotionDiff(w, "code", "wardyn.example.com").plumbing).toEqual(["wardyn.example.com"]);
      expect(egressPromotionDiff(w, "code", "wardyn.example.com").approvable).toEqual([]);
      // Omitted selfHost (this file's DOM-free contract — see the header
      // comment — means callers must supply it) degrades to NOT excluding
      // it, never a crash.
      expect(egressPromotionDiff(w, "code").approvable).toEqual(["wardyn.example.com"]);
    });

    it("a host already covered by approved_egress/profile/requirements is alreadyApproved, never plumbing", () => {
      const rr: RecordResult = {
        run_id: "r1",
        mode: "auto",
        status: "recorded",
        observations: {
          domains: [{ host: "github.com", allow_count: 1, deny_count: 0, pending_count: 0 }],
          minted_grant_ids: [],
          exec_argv0s: [],
          file_writes: [],
          connects: [],
          anomalies: [],
        },
      };
      const w = ws({ record_results: { code: rr }, approved_egress: ["github.com"] });
      const diff = egressPromotionDiff(w, "code");
      expect(diff.alreadyApproved).toEqual(["github.com"]);
      expect(diff.plumbing).toEqual([]);
      expect(diff.approvable).toEqual([]);
    });

    it("every observed+allowed host lands in exactly one bucket (no drift, no double-count)", () => {
      const rr: RecordResult = {
        run_id: "r1",
        mode: "auto",
        status: "recorded",
        observations: {
          domains: [
            { host: "registry.npmjs.org", allow_count: 1, deny_count: 0, pending_count: 0 }, // approvable
            { host: "github.com", allow_count: 1, deny_count: 0, pending_count: 0 }, // already approved
            { host: "api.anthropic.com", allow_count: 1, deny_count: 0, pending_count: 0 }, // plumbing
            { host: "blocked.example", allow_count: 0, deny_count: 5, pending_count: 0 }, // never observed-allowed at all
          ],
          minted_grant_ids: [],
          exec_argv0s: [],
          file_writes: [],
          connects: [],
          anomalies: [],
        },
      };
      const w = ws({ record_results: { code: rr }, approved_egress: ["github.com"] });
      const diff = egressPromotionDiff(w, "code");
      const all = [...diff.approvable, ...diff.alreadyApproved, ...diff.plumbing];
      expect(all.sort()).toEqual(["api.anthropic.com", "github.com", "registry.npmjs.org"]);
      expect(new Set(all).size).toBe(all.length); // no host counted twice
    });
  });

  it("isEmptyCapture is true when a settled recording observed no egress", () => {
    expect(isEmptyCapture(undefined)).toBe(false);
    expect(isEmptyCapture({ run_id: "r", mode: "auto", status: "record_failed" })).toBe(true);
    expect(
      isEmptyCapture({
        run_id: "r",
        mode: "auto",
        status: "recorded",
        observations: { domains: [], minted_grant_ids: [], exec_argv0s: [], file_writes: [], connects: [], anomalies: [] },
      }),
    ).toBe(true);
    expect(
      isEmptyCapture({
        run_id: "r",
        mode: "auto",
        status: "recorded",
        observations: {
          domains: [{ host: "x", allow_count: 1, deny_count: 0, pending_count: 0 }],
          minted_grant_ids: [],
          exec_argv0s: [],
          file_writes: [],
          connects: [],
          anomalies: [],
        },
      }),
    ).toBe(false);
  });
});

// B3: the workspace-wide roll-up — the latest settled confined entry whose
// server verdict was clean===true, across every session.
describe("lastCleanReplay — the workspace-wide roll-up", () => {
  it("undefined when no session has ever replayed clean", () => {
    expect(lastCleanReplay(ws())).toBeUndefined();
    const w = ws({
      record_results: {
        s: { run_id: "r", mode: "interactive", status: "recorded" }, // open — never eligible
        "verify:s": { run_id: "r2", mode: "interactive", confined: true, status: "recorded", clean: false, caught: 1 },
      },
    });
    expect(lastCleanReplay(w)).toBeUndefined();
  });

  it("picks the LATEST clean confined entry by finished_at, across different sessions", () => {
    const w = ws({
      record_results: {
        a: { run_id: "ra", mode: "interactive", status: "recorded" },
        "verify:a": {
          run_id: "ra2", label: "build & test", mode: "interactive", confined: true, status: "recorded",
          clean: true, caught: 0, finished_at: "2026-01-01T00:00:00Z",
        },
        b: { run_id: "rb", mode: "interactive", status: "recorded" },
        "verify:b": {
          run_id: "rb2", label: "agent loop", mode: "interactive", confined: true, status: "recorded",
          clean: true, caught: 0, finished_at: "2026-01-03T00:00:00Z", // latest
        },
      },
    });
    expect(lastCleanReplay(w)).toEqual({ label: "agent loop", finishedAt: "2026-01-03T00:00:00Z" });
  });

  it("excludes a non-confined (open) entry, a not-clean entry, and a still-in-flight entry", () => {
    const w = ws({
      record_results: {
        s: { run_id: "r1", mode: "interactive", status: "recorded", clean: true, caught: 0 }, // open: clean is meaningless here
        t: { run_id: "r2", mode: "interactive", status: "recorded" },
        "verify:t": { run_id: "r3", mode: "interactive", confined: true, status: "recorded", clean: false, caught: 0 },
        u: { run_id: "r4", mode: "interactive", status: "recorded" },
        "verify:u": { run_id: "r5", mode: "interactive", confined: true, status: "recording", clean: true, caught: 0 },
      },
    });
    expect(lastCleanReplay(w)).toBeUndefined();
  });

  it("falls back to the key (verify: prefix stripped) when the entry carries no label", () => {
    const w = ws({
      record_results: {
        "verify:build-test": { run_id: "r", mode: "interactive", confined: true, status: "recorded", clean: true, caught: 0 },
      },
    });
    expect(lastCleanReplay(w)).toEqual({ label: "build-test", finishedAt: undefined });
  });
});

describe("sessionStage — record open → recorded → replaying confined → replayed", () => {
  it("recording: the open capture is still in flight", () => {
    const w = ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "recording" } } });
    expect(sessionStage(w, "s")).toBe("recording");
  });

  it("recorded: the open capture settled and no confined replay exists yet", () => {
    const w = ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "recorded" } } });
    expect(sessionStage(w, "s")).toBe("recorded");
  });

  it("record_failed: the open capture failed and no confined replay exists yet", () => {
    const w = ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "record_failed" } } });
    expect(sessionStage(w, "s")).toBe("record_failed");
  });

  it("replaying: a confined replay of a recorded session is in flight", () => {
    const w = ws({
      record_results: {
        s: { run_id: "r", mode: "interactive", status: "recorded" },
        "verify:s": { run_id: "r2", mode: "interactive", confined: true, status: "recording" },
      },
    });
    expect(sessionStage(w, "s")).toBe("replaying");
  });

  it("replayed: the confined replay settled", () => {
    const w = ws({
      record_results: {
        s: { run_id: "r", mode: "interactive", status: "recorded" },
        "verify:s": { run_id: "r2", mode: "interactive", confined: true, status: "recorded" },
      },
    });
    expect(sessionStage(w, "s")).toBe("replayed");
  });

  // W20-capture-store-1: a re-recorded session must NOT inherit the PRIOR
  // recording's confined verdict — verify:<key> is keyed only on the session
  // key, so it silently survives a re-record untouched.
  it("re-record after replay: the stale confined verdict must not outrank the fresh, unverified open capture", () => {
    // record session K (t1) -> replay confined (t2, passes) -> RE-record K (t3,
    // fresh open, never replayed). The verify:s entry is still the t2 replay of
    // the ORIGINAL (t1) capture.
    const w = ws({
      record_results: {
        s: { run_id: "r3", mode: "interactive", status: "recorded", started_at: "2026-01-02T00:00:00Z" },
        "verify:s": { run_id: "r2", mode: "interactive", confined: true, status: "recorded", started_at: "2026-01-01T01:00:00Z" },
      },
    });
    // NOT the stale "replayed" green — the fresh capture reads as needing a replay.
    expect(sessionStage(w, "s")).toBe("recorded");
  });

  it("re-record after replay, re-record FAILS: the failed fresh open still outranks the stale confined verdict", () => {
    const w = ws({
      record_results: {
        s: { run_id: "r3", mode: "interactive", status: "record_failed", started_at: "2026-01-02T00:00:00Z" },
        "verify:s": { run_id: "r2", mode: "interactive", confined: true, status: "recorded", started_at: "2026-01-01T01:00:00Z" },
      },
    });
    expect(sessionStage(w, "s")).toBe("record_failed");
  });

  it("legit flow preserved: replaying the NEW capture after a re-record still shows the green verdict", () => {
    // record K (t1) -> replay (t2) -> re-record K (t3) -> replay AGAIN (t4):
    // the confined entry now postdates the current open capture, so it's current.
    const w = ws({
      record_results: {
        s: { run_id: "r3", mode: "interactive", status: "recorded", started_at: "2026-01-02T00:00:00Z" },
        "verify:s": { run_id: "r4", mode: "interactive", confined: true, status: "recorded", started_at: "2026-01-02T01:00:00Z" },
      },
    });
    expect(sessionStage(w, "s")).toBe("replayed");
  });
});
