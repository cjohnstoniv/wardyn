/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Finding 7b (0.7.5 field report, Codex #8/#9): the markerless half of the
// sign-in confirmation — extractSignedIn (the CLI's own hint), the strict
// corroboration `serverConfirmsCapture` gained, and watchForCapture, the
// background watch that is the only NEW way this lane lets the pane reach
// onDone without a marker.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  extractSignedIn,
  serverConfirmsCapture,
  watchForCapture,
  CAPTURE_POST_RUN_GRACE_MS,
  CAPTURE_WATCH_MAX_MS,
} from "./capture-confirm";
import type { AgentRun, SetupStatus } from "../../../lib/types";

const getRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({ runs: { getRun: (...a: unknown[]) => getRunMock(...a) } }));
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) } }));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));

function status(overrides: Partial<SetupStatus>): SetupStatus {
  return {
    ready: true,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: [] },
    providers: [],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false },
    ...overrides,
  } as SetupStatus;
}

describe("extractSignedIn", () => {
  it("matches the CLI's own success line, case-insensitively", () => {
    expect(extractSignedIn("Successfully logged into Start URL\n")).toBe(true);
    expect(extractSignedIn("SUCCESSFULLY LOGGED INTO start url\n")).toBe(true);
  });

  it("does not match before the line appears", () => {
    expect(extractSignedIn("Attempting to automatically open the SSO authorization page\n")).toBe(false);
  });

  it("matches embedded in noisier output (loose, not anchored)", () => {
    expect(extractSignedIn("...\r\nsuccessfully logged into Start URL\r\nwardyn: sign-in running\n")).toBe(true);
  });
});

describe("serverConfirmsCapture — strict (Codex #9)", () => {
  // THE most important case in the lane: a free-running watch must never
  // fall through to the presence fallbacks — a pre-existing live
  // model_access would otherwise confirm a sign-in that never happened.
  it("refuses a live model_access with no matching source_run_id", () => {
    expect(
      serverConfirmsCapture(status({ model_access: { state: "live" } }), "aws", "run-123", { strict: true }),
    ).toBe(false);
  });

  it("refuses a captured harness row with no runId to compare at all", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true }] }),
        "aws",
        null,
        { strict: true },
      ),
    ).toBe(false);
  });

  it("still confirms a harness row stamped with THIS run", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }),
        "aws",
        "run-123",
        { strict: true },
      ),
    ).toBe(true);
  });

  // Regression: every existing (non-strict) caller — confirmCaptureWithServer,
  // every S-13 pin in the sibling test file — is unchanged by the new option.
  it("non-strict (the default) keeps the presence fallbacks", () => {
    expect(serverConfirmsCapture(status({ model_access: { state: "live" } }), "aws", "run-123")).toBe(true);
  });
});

describe("watchForCapture (Finding 7b, Codex #8/#9)", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    getRunMock.mockReset().mockResolvedValue({ id: "run-123", state: "RUNNING" } as AgentRun);
    listAuditMock.mockReset().mockResolvedValue([]);
    getSetupStatusMock.mockReset().mockResolvedValue(status({}));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("confirms and stops the moment the audit hint fires and /setup/status agrees", async () => {
    listAuditMock.mockResolvedValue([{ id: "a1", action: "harness.credential.captured" }]);
    getSetupStatusMock.mockResolvedValue(
      status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }),
    );
    const controller = new AbortController();
    const result = await watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    expect(result).toBe(true);
    // The hint made the FIRST tick authoritative — no 30s fallback needed.
    expect(getSetupStatusMock).toHaveBeenCalledTimes(1);
  });

  it("a thrown audit read is a tick, not a verdict — the 30s status fallback still confirms", async () => {
    listAuditMock.mockRejectedValue(new Error("network error"));
    getSetupStatusMock
      .mockResolvedValueOnce(status({})) // the immediate first fallback check, at watch start? no hint yet
      .mockResolvedValueOnce(status({}))
      .mockResolvedValue(status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }));
    const controller = new AbortController();
    const promise = watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    await vi.advanceTimersByTimeAsync(2 * 60_000);
    await expect(promise).resolves.toBe(true);
  });

  it("an unreachable /setup/status payload is a tick, not a refusal", async () => {
    getSetupStatusMock
      .mockResolvedValueOnce({ unreachable: true, ready: true } as unknown as SetupStatus)
      .mockResolvedValueOnce({ unreachable: true, ready: true } as unknown as SetupStatus)
      .mockResolvedValue(status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }));
    listAuditMock.mockResolvedValue([{ id: "a1" }]); // hint every tick — fast cadence
    const controller = new AbortController();
    const promise = watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    await vi.advanceTimersByTimeAsync(30_000);
    await expect(promise).resolves.toBe(true);
  });

  it("a thrown /setup/status read is also a tick, not a refusal", async () => {
    listAuditMock.mockResolvedValue([{ id: "a1" }]);
    getSetupStatusMock
      .mockRejectedValueOnce(new Error("401"))
      .mockResolvedValue(status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }));
    const controller = new AbortController();
    const promise = watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    await vi.advanceTimersByTimeAsync(10_000);
    await expect(promise).resolves.toBe(true);
  });

  it("expires CAPTURE_POST_RUN_GRACE_MS after the run goes terminal", async () => {
    getRunMock.mockResolvedValue({ id: "run-123", state: "COMPLETED" } as AgentRun);
    listAuditMock.mockResolvedValue([]); // never hinted
    getSetupStatusMock.mockResolvedValue(status({})); // never confirms
    const controller = new AbortController();
    const promise = watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    await vi.advanceTimersByTimeAsync(CAPTURE_POST_RUN_GRACE_MS + 60_000);
    await expect(promise).resolves.toBe(false);
  });

  it("expires at the absolute CAPTURE_WATCH_MAX_MS even under continuous activity", async () => {
    getRunMock.mockResolvedValue({ id: "run-123", state: "RUNNING" } as AgentRun); // never terminal
    listAuditMock.mockResolvedValue([{ id: "a1" }]); // hinted every tick — busy, but never confirming
    getSetupStatusMock.mockResolvedValue(status({})); // never confirms
    const controller = new AbortController();
    const promise = watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    await vi.advanceTimersByTimeAsync(CAPTURE_WATCH_MAX_MS + 5 * 60_000);
    await expect(promise).resolves.toBe(false);
  });

  it("a persistent read failure also ends at the absolute deadline", async () => {
    getRunMock.mockRejectedValue(new Error("control plane unreachable"));
    listAuditMock.mockRejectedValue(new Error("control plane unreachable"));
    getSetupStatusMock.mockRejectedValue(new Error("control plane unreachable"));
    const controller = new AbortController();
    const promise = watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    await vi.advanceTimersByTimeAsync(CAPTURE_WATCH_MAX_MS + 5 * 60_000);
    await expect(promise).resolves.toBe(false);
  });

  it("an already-aborted signal ends the watch at once, with no reads", async () => {
    const controller = new AbortController();
    controller.abort();
    const result = await watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal });
    expect(result).toBe(false);
    expect(getSetupStatusMock).not.toHaveBeenCalled();
  });

  // review-1 S2: `wake` cuts the CURRENT back-off wait short — no fake-timer
  // advance needed at all, because dispatching the event resolves the
  // pending sleep synchronously (the caller wires this to the CLI-line hint
  // and to `visibilitychange`; the run's own state transition needs no
  // separate wiring, since the loop already re-reads `getRun` every tick).
  it("a wake() tick ends the wait immediately, without advancing any timer", async () => {
    getRunMock.mockResolvedValue({ id: "run-123", state: "RUNNING" } as AgentRun);
    let auditCalls = 0;
    listAuditMock.mockImplementation(async () => {
      auditCalls += 1;
      return auditCalls >= 2 ? [{ id: "a1" }] : []; // no hint on tick 1, hinted from tick 2
    });
    getSetupStatusMock.mockResolvedValue(
      status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }),
    );
    const controller = new AbortController();
    const wake = new EventTarget();
    const promise = watchForCapture({ provider: "aws", runId: "run-123", signal: controller.signal, wake });

    // Let the FIRST tick's reads settle (no hint yet, so it schedules a 2s
    // sleep) without advancing time, then wake it.
    await vi.advanceTimersByTimeAsync(0);
    wake.dispatchEvent(new Event("tick"));

    await expect(promise).resolves.toBe(true);
  });
});
