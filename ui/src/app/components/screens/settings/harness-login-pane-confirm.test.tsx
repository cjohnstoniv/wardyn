/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Finding 7b (0.7.5 field report): the pane sits on a sign-in that worked.
//
// Its own file because harness-login-pane.test.tsx is at the 1000-line gate
// (the same split `-launch.test.tsx` already established) — and because the
// 944-line sibling holds the 19 S-13 / serverConfirmsCapture pins and gets NO
// new cases (the lane brief, COMMON.md's regression law): every case here is
// NEW behaviour, never a rewrite of what that file already pins.
//
// review-1 S1 — Codex #9 design point 3 STANDS (revised from this lane's
// first pass, which had deferred it — see REVIEW-1.md): confirmCapture's own
// short round trip (confirmCaptureWithServer, ~1.5s) no longer refuses a
// mismatch by itself. It remembers the sentence and hands off to the
// background watch (already running independently since `attached`), which
// keeps CAPTURE_VERIFYING (+ Cancel) on screen and is what actually ends the
// wait — at its own bound (terminal + 5 min grace, or 45 min absolute), not
// at 1.5s. The sibling 944-line file's four S-13 timing pins were rewritten
// under fake timers to match (see that file and this lane's REPORT).
import * as React from "react";
import { act } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

let lastAttachOutput: ((chunk: string) => void) | undefined;
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(
    props: { onOutput?: (chunk: string) => void; autoRun?: string },
    ref: React.ForwardedRef<{ sendText: (t: string) => void }>,
  ) {
    lastAttachOutput = props.onOutput;
    React.useImperativeHandle(ref, () => ({ sendText: () => {} }), []);
    return <div data-testid="fake-terminal" />;
  }),
}));
const harnessLoginMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: { harnessLogin: (...a: unknown[]) => harnessLoginMock(...a), harnessCredentialPaste: vi.fn() },
}));
const killRunMock = vi.fn();
const getRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: (...a: unknown[]) => killRunMock(...a), getRun: (...a: unknown[]) => getRunMock(...a) } }));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) } }));
// A real window.open in jsdom is "not implemented" and returns undefined
// anyway (auth-tab-handle.test.ts covers the tab lifecycle itself); every
// case here only needs it not to throw.
vi.spyOn(window, "open").mockReturnValue(null);

import { HarnessLoginPane } from "./harness-login-pane";
import { CAPTURE_HANDOFF, CAPTURE_NOT_CORROBORATED, CAPTURE_POST_RUN_GRACE_MS } from "./capture-confirm";
import type { AgentRun, SetupStatus } from "../../../lib/types";

// The aws-sso helper's own success marker (login-flows.tsx's `doneMarker`).
const DONE_MARKER = "wardyn: aws sso credential captured";

// A generous but bounded sample of the watch's own schedule — long enough for
// its fast (2s) tier to have ticked several times, short enough to keep the
// case fast under fake timers.
const CAPTURE_WATCH_SAMPLE_MS = 10_000;

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

async function attachAwsRun(onDone = vi.fn(), onCancel = vi.fn()) {
  render(<HarnessLoginPane provider="aws" startURLManaged onDone={onDone} onCancel={onCancel} />);
  await userEvent.click(screen.getByRole("button", { name: /start login/i }));
  await screen.findByTestId("fake-terminal");
  return { onDone, onCancel };
}

describe("HarnessLoginPane — the CLI's own success line (Finding 7b)", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    killRunMock.mockReset().mockResolvedValue(undefined);
    getRunMock.mockReset().mockResolvedValue({ id: "run-123", state: "RUNNING" } as AgentRun);
    getSetupStatusMock.mockReset().mockResolvedValue(status({}));
    listAuditMock.mockReset().mockResolvedValue([]);
    lastAttachOutput = undefined;
  });

  it("replaces the instructive blurb with CAPTURE_HANDOFF once the CLI reports success", async () => {
    await attachAwsRun();
    expect(screen.getByTestId("helper-flow-note")).not.toHaveTextContent(CAPTURE_HANDOFF);

    await act(async () => lastAttachOutput?.("Successfully logged into Start URL\n"));

    expect(screen.getByTestId("helper-flow-note")).toHaveTextContent(CAPTURE_HANDOFF);
    // A hint, not a verdict: no phase change, no capture claimed yet.
    expect(screen.queryByText(/session captured/i)).not.toBeInTheDocument();
  });

  // THE HEADLINE: a capture the server confirms closes the pane though the
  // marker never arrived at all — the markerless watch, not the CLI hint,
  // is what ends this (the hint only swapped the sentence above).
  it("closes the pane on a confirmed capture though the marker never arrives", async () => {
    const { onDone } = await attachAwsRun();
    listAuditMock.mockResolvedValue([{ id: "a1", action: "harness.credential.captured" }]);
    getSetupStatusMock.mockResolvedValue(
      status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }),
    );

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });

    expect(onDone).toHaveBeenCalledTimes(1);
    expect(killRunMock).toHaveBeenCalledWith("run-123");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  // R-5 / Codex #9: the strict predicate, proven end to end through the
  // component — a credential that predates THIS sign-in must never close
  // the pane over a capture that did not happen.
  it("does NOT close the pane on a credential from a different run", async () => {
    const { onDone } = await attachAwsRun();
    listAuditMock.mockResolvedValue([{ id: "a1" }]);
    getSetupStatusMock.mockResolvedValue(
      status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-earlier" }], model_access: { state: "live" } }),
    );

    await act(async () => {
      await vi.advanceTimersByTimeAsync(CAPTURE_WATCH_SAMPLE_MS);
    });

    expect(onDone).not.toHaveBeenCalled();
  });

  it("stops watching once the operator cancels — no late onDone", async () => {
    const { onDone, onCancel } = await attachAwsRun();
    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(onCancel).toHaveBeenCalled();

    listAuditMock.mockResolvedValue([{ id: "a1" }]);
    getSetupStatusMock.mockResolvedValue(
      status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(CAPTURE_WATCH_SAMPLE_MS);
    });

    expect(onDone).not.toHaveBeenCalled();
  });

  // The two plan tests (Codex #9, design point 3).
  it("a marker whose corroboration fails keeps verifying instead of refusing", async () => {
    const { onDone } = await attachAwsRun();
    getSetupStatusMock.mockResolvedValue(status({})); // never confirms

    await act(async () => lastAttachOutput?.(`${DONE_MARKER}\n`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });

    expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /cancel/i })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(onDone).not.toHaveBeenCalled();
  });

  it("a forged marker ends in CAPTURE_NOT_CORROBORATED at the watch's expiry", async () => {
    const { onDone } = await attachAwsRun();
    getSetupStatusMock.mockResolvedValue(status({})); // never confirms
    listAuditMock.mockResolvedValue([]); // never hinted

    await act(async () => lastAttachOutput?.(`${DONE_MARKER}\n`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });
    getRunMock.mockResolvedValue({ id: "run-123", state: "COMPLETED" } as AgentRun);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(CAPTURE_POST_RUN_GRACE_MS + 60_000);
    });

    const alertBox = screen.getByRole("alert");
    expect(alertBox).toHaveTextContent(CAPTURE_NOT_CORROBORATED);
    expect(killRunMock).toHaveBeenCalledWith("run-123");
    expect(onDone).not.toHaveBeenCalled();
  });

  // Both the marker path (confirmCapture) and the background
  // watch can independently reach a confirming status around the same tick
  // — `completedRef` inside completeCapture must let only the FIRST one act.
  it("a race between the marker and the watch converges on exactly one cleanup and one onDone", async () => {
    const { onDone } = await attachAwsRun();
    listAuditMock.mockResolvedValue([{ id: "a1", action: "harness.credential.captured" }]);
    getSetupStatusMock.mockResolvedValue(
      status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }),
    );

    // The CLI marker fires confirmCapture's own round trip; the watch,
    // already running since `attached`, is independently hinted on the same
    // tick and reads the SAME confirming status.
    await act(async () => lastAttachOutput?.(`${DONE_MARKER}\n`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });

    expect(onDone).toHaveBeenCalledTimes(1);
    expect(killRunMock).toHaveBeenCalledTimes(1);
  });
});
