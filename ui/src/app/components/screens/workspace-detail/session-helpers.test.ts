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
  sessionKeyOf,
  recordResult,
  isRecording,
  newEgressHosts,
  isEmptyCapture,
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
});
