/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  IMPORT_STEPS,
  activeStepForStatus,
  isTransientStatus,
  verifyPhase,
  verifyRows,
  verifyProgress,
  runningLabel,
  fmtStepDuration,
  recordSessions,
  sessionKeyOf,
  recordResult,
  isRecording,
  newEgressHosts,
  isEmptyCapture,
} from "./import-types";
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

describe("activeStepForStatus", () => {
  it("maps each server status to the right rail step", () => {
    expect(activeStepForStatus("pending_scan")).toBe("scan");
    expect(activeStepForStatus("scanning")).toBe("scan");
    expect(activeStepForStatus("error")).toBe("scan");
    expect(activeStepForStatus("scanned")).toBe("configure");
    expect(activeStepForStatus("building")).toBe("verify");
    expect(activeStepForStatus("build_error")).toBe("verify");
    expect(activeStepForStatus("verifying")).toBe("verify");
    expect(activeStepForStatus("verify_failed")).toBe("verify");
    expect(activeStepForStatus("ready")).toBe("finalize");
  });
});

describe("isTransientStatus", () => {
  it("is true only for in-flight server states", () => {
    expect(isTransientStatus("scanning")).toBe(true);
    expect(isTransientStatus("building")).toBe(true);
    expect(isTransientStatus("verifying")).toBe(true);
    expect(isTransientStatus("scanned")).toBe(false);
    expect(isTransientStatus("ready")).toBe(false);
    expect(isTransientStatus("verify_failed")).toBe(false);
  });
});

describe("verifyPhase (fixed vocabulary)", () => {
  it("is Verifying while building/verifying", () => {
    expect(verifyPhase("building")).toBe("verifying");
    expect(verifyPhase("verifying")).toBe("verifying");
  });
  it("is Success when the verify ran and every step passed", () => {
    expect(verifyPhase("ready", { ran: true, ok: true, steps: [] })).toBe("success");
    expect(verifyPhase("ready")).toBe("success");
  });
  it("is Partial when it ran, failed overall, but some step passed", () => {
    expect(
      verifyPhase("verify_failed", {
        ran: true,
        ok: false,
        steps: [
          { stage: "install", command: "npm ci", exit_code: 0 },
          { stage: "build", command: "npm run build", exit_code: 1 },
        ],
      }),
    ).toBe("partial");
  });
  it("is Failed when it ran and no step passed", () => {
    expect(
      verifyPhase("verify_failed", {
        ran: true,
        ok: false,
        steps: [{ stage: "install", command: "npm ci", exit_code: 1 }],
      }),
    ).toBe("failed");
  });
});

describe("verifyRows — merge approved commands with streamed steps", () => {
  const commands = [
    { stage: "install", command: "npm ci" },
    { stage: "build", command: "npm run build" },
    { stage: "test", command: "npm test" },
  ];

  it("flags done/running/pending by order (mid-flight)", () => {
    const rows = verifyRows(commands, {
      ran: true,
      ok: false,
      done: false,
      total: 3,
      steps: [
        { stage: "install", command: "npm ci", exit_code: 0 },
        { stage: "build", command: "npm run build", running: true, exit_code: -1 },
      ],
    });
    expect(rows.map((r) => r.state)).toEqual(["pass", "running", "pending"]);
    // The pending row falls back to the approved command (no result entry yet).
    expect(rows[2]).toMatchObject({ stage: "test", command: "npm test" });
    expect(rows[2].step).toBeUndefined();
  });

  it("marks a nonzero exit or a timeout as fail", () => {
    const rows = verifyRows(commands, {
      ran: true,
      ok: false,
      steps: [
        { stage: "install", command: "npm ci", exit_code: 0, timed_out: true },
        { stage: "build", command: "npm run build", exit_code: 2 },
      ],
    });
    expect(rows[0].state).toBe("fail"); // exit 0 but timed out
    expect(rows[1].state).toBe("fail");
  });

  it("renders extra result steps that outrun the approved list (degraded shape)", () => {
    const rows = verifyRows([], {
      ran: true,
      ok: false,
      steps: [{ stage: "build", command: "npm run build", exit_code: 1 }],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ state: "fail", command: "npm run build" });
  });

  it("is empty for no commands and no result", () => {
    expect(verifyRows([])).toEqual([]);
  });
});

describe("verifyProgress — step N of total", () => {
  const commands = [
    { stage: "install", command: "npm ci" },
    { stage: "build", command: "npm run build" },
    { stage: "test", command: "npm test" },
  ];

  it("counts started steps against verify_result.total", () => {
    expect(
      verifyProgress(commands, { ran: true, ok: false, total: 3, steps: [{ stage: "install", command: "npm ci", exit_code: 0 }, { stage: "build", command: "npm run build", running: true, exit_code: -1 }] }),
    ).toEqual({ started: 2, total: 3 });
  });

  it("falls back to the approved command count when total is absent", () => {
    expect(verifyProgress(commands, { ran: true, ok: false, steps: [] })).toEqual({ started: 0, total: 3 });
  });

  it("never reports fewer total than started (degraded shape)", () => {
    expect(
      verifyProgress([], { ran: true, ok: false, steps: [{ stage: "build", command: "npm run build", exit_code: 1 }] }),
    ).toEqual({ started: 1, total: 1 });
  });
});

describe("runningLabel + fmtStepDuration", () => {
  it("maps known stages to a gerund and falls back to the command", () => {
    expect(runningLabel({ stage: "install", command: "npm ci" })).toBe("Installing dependencies…");
    expect(runningLabel({ stage: "build", command: "x" })).toBe("Building…");
    expect(runningLabel({ stage: "whatever", command: "make thing" })).toBe("Running make thing…");
  });
  it("formats durations compactly (blank under 1s)", () => {
    expect(fmtStepDuration(12000)).toBe("12s");
    expect(fmtStepDuration(65000)).toBe("1m 5s");
    expect(fmtStepDuration(500)).toBe("");
    expect(fmtStepDuration(undefined)).toBe("");
  });
});

describe("IMPORT_STEPS — Record sits between Configure and Verify", () => {
  it("orders the rail source→scan→configure→record→verify→finalize", () => {
    expect(IMPORT_STEPS.map((s) => s.id)).toEqual([
      "source",
      "scan",
      "configure",
      "record",
      "verify",
      "finalize",
    ]);
  });
});

describe("record helpers — read the server-authored record fields", () => {
  it("recordSessions lists record_results (key + label), ordered by start time", () => {
    expect(recordSessions(ws())).toEqual([]);
    const w = ws({
      record_results: {
        "agent-loop": { run_id: "r2", label: "agent loop", mode: "interactive", status: "recorded", started_at: "2026-01-02" },
        "build-test": { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded", started_at: "2026-01-01" },
        "no-label": { run_id: "r3", mode: "interactive", status: "recorded", started_at: "2026-01-03" },
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

  it("recordResult reads straight off the workspace (never derive)", () => {
    const rr: RecordResult = { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded" };
    const w = ws({ record_results: { "build-test": rr } });
    expect(recordResult(w, "build-test")).toBe(rr);
    expect(recordResult(w, "missing")).toBeUndefined();
  });

  it("isRecording is true iff any session's run is still in flight", () => {
    expect(isRecording(ws())).toBe(false);
    expect(isRecording(ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "recorded" } } }))).toBe(false);
    expect(
      isRecording(ws({ record_results: { s: { run_id: "r", mode: "interactive", status: "recording" } } })),
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
