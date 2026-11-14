/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's recording index: each list keeps its own last-good rows, saved desktop recordings are
// read by their exact action so pause and resume rows cannot crowd them off the page, and the Recording
// tab says when the index may be missing recordings.
import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../../lib/types";

type Filter = { action?: string; actionPrefix?: string } | undefined;
const answer = vi.hoisted(() => ({ fn: (_f: Filter): Promise<unknown[]> => Promise.resolve([]) }));
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getRun: (id: string) => Promise.resolve({ id, state: "COMPLETED", task: "t", agent: "a" }),
    getGrants: () => Promise.resolve([]),
  },
}));
vi.mock("../../../lib/api/approvals", () => ({ approvals: { listApprovals: () => Promise.resolve([]) } }));
vi.mock("../../../lib/api/audit", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/audit")>()),
  audit: { listAudit: (_id: string, f?: Filter) => answer.fn(f) },
}));
vi.mock("../../../lib/api/recordings", () => ({ recordings: { getRecording: () => Promise.resolve(null) } }));
vi.mock("../../../lib/api/health", () => ({ health: { health: () => Promise.resolve({}) } }));

import { LIST_LIMIT } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { RECORDING_INDEX_INCOMPLETE } from "../../wardyn/copy/lookup-failures";
import { desktopSessions } from "./desktop-replay";
import { RecordingTab } from "./recording-tab";
import { useRunDetail } from "./use-run-detail";

const row = (id: string, action: string, target = "run-1"): AuditEvent =>
  ({ id, time: aheadByHours(-1), actor_type: "system", actor: "wardynd", action, target, outcome: "success" }) as AuditEvent;
const DESKTOP = row("w1", "desktop.recording.write", "run-1~desktop-a");
const CAST = row("s1", "session.recording.write", "run-1~console-1");
const isDesktop = (f: Filter) => f?.action === "desktop.recording.write" || f?.actionPrefix === "desktop.recording";
const isSessions = (f: Filter) => f?.actionPrefix === "session.recording";
const desktopKeys = (rows: AuditEvent[]) => desktopSessions(rows).map((e) => e.target);

function hook(id = "run-1") {
  return renderHook(({ runId }) => useRunDetail(runId), {
    initialProps: { runId: id },
    wrapper: ({ children }) => <MemoryRouter>{children}</MemoryRouter>,
  });
}

beforeEach(() => {
  answer.fn = () => Promise.resolve([]);
});

describe("useRunDetail recording index", () => {
  it("keeps the desktop recordings it read when the terminal session read fails", async () => {
    answer.fn = (f) => (isSessions(f) ? Promise.reject(new Error("audit store down")) : Promise.resolve(isDesktop(f) ? [DESKTOP] : []));
    const { result } = hook();
    await waitFor(() => expect(result.current.status).toBe("ready"));
    expect(desktopKeys(result.current.recordingAudit)).toEqual(["run-1~desktop-a"]);
    expect(result.current.recordingIndex).toBe("failed");
  });

  it("keeps last-good desktop rows when a later desktop read fails, and clears the failure once it reads", async () => {
    answer.fn = (f) => Promise.resolve(isDesktop(f) ? [DESKTOP] : isSessions(f) ? [CAST] : []);
    const { result } = hook();
    await waitFor(() => expect(desktopKeys(result.current.recordingAudit)).toEqual(["run-1~desktop-a"]));
    expect(result.current.recordingIndex).toBe("complete");

    answer.fn = (f) => (isDesktop(f) ? Promise.reject(new Error("audit store down")) : Promise.resolve(isSessions(f) ? [CAST] : []));
    await act(async () => {
      await result.current.load(false);
    });
    expect(desktopKeys(result.current.recordingAudit)).toEqual(["run-1~desktop-a"]);
    expect(result.current.recordingAudit.map((e) => e.id).sort()).toEqual(["s1", "w1"]);
    expect(result.current.recordingIndex).toBe("failed");

    answer.fn = (f) => Promise.resolve(isDesktop(f) ? [DESKTOP] : isSessions(f) ? [CAST] : []);
    await act(async () => {
      await result.current.load(false);
    });
    expect(result.current.recordingIndex).toBe("complete");
  });

  it("a failed gap read alone hides no recording", async () => {
    answer.fn = (f) =>
      f?.actionPrefix === "desktop.recording"
        ? Promise.reject(new Error("audit store down"))
        : Promise.resolve(f?.action === "desktop.recording.write" ? [DESKTOP] : []);
    const { result } = hook();
    await waitFor(() => expect(desktopKeys(result.current.recordingAudit)).toEqual(["run-1~desktop-a"]));
    expect(result.current.recordingIndex).toBe("complete");
  });

  it("finds the saved desktop recording after a full page of pause and resume rows", async () => {
    const gaps = Array.from({ length: LIST_LIMIT }, (_, i) =>
      row(`g${i}`, i % 2 ? "desktop.recording.resume" : "desktop.recording.pause"),
    );
    answer.fn = (f) =>
      Promise.resolve(f?.action === "desktop.recording.write" ? [DESKTOP] : f?.actionPrefix === "desktop.recording" ? gaps : []);
    const { result } = hook();
    await waitFor(() => expect(desktopKeys(result.current.recordingAudit)).toEqual(["run-1~desktop-a"]));
    expect(result.current.recordingIndex).toBe("complete");
  });

  it("says the index may be missing recordings when a list of them fills its page", async () => {
    const writes = Array.from({ length: LIST_LIMIT }, (_, i) => row(`w${i}`, "desktop.recording.write", `run-1~desktop-${i}`));
    answer.fn = (f) => Promise.resolve(f?.action === "desktop.recording.write" ? writes : []);
    const { result } = hook();
    await waitFor(() => expect(result.current.recordingIndex).toBe("capped"));
  });

  it("drops the last run's recording rows when the page moves to another run", async () => {
    answer.fn = (f) => Promise.resolve(isDesktop(f) ? [DESKTOP] : []);
    const { result, rerender } = hook();
    await waitFor(() => expect(desktopKeys(result.current.recordingAudit)).toEqual(["run-1~desktop-a"]));

    answer.fn = (f) => (isDesktop(f) ? Promise.reject(new Error("audit store down")) : Promise.resolve([]));
    rerender({ runId: "run-2" });
    await waitFor(() => expect(result.current.run?.id).toBe("run-2"));
    expect(result.current.recordingAudit).toEqual([]);
    expect(result.current.recordingIndex).toBe("failed");
  });
});

describe("RecordingTab incomplete index note", () => {
  const tab = (props: Partial<React.ComponentProps<typeof RecordingTab>>) =>
    render(
      <MemoryRouter>
        <RecordingTab state="ready" recording={null} recordingDisabled={false} runId="run-1" sessions={[]} selected="run-1"
          onSelect={() => {}} onRetry={() => {}} {...props} />
      </MemoryRouter>,
    );

  it("says so and offers a retry when the index read failed", () => {
    const onRetryIndex = vi.fn();
    tab({ recordingIndex: "failed", onRetryIndex });
    expect(screen.getByText(RECORDING_INDEX_INCOMPLETE.NOTE)).toBeInTheDocument();
    const retry = screen.getByRole("button", { name: RECORDING_INDEX_INCOMPLETE.RETRY_LABEL });
    expect(retry).toHaveTextContent(RECORDING_INDEX_INCOMPLETE.RETRY);
    fireEvent.click(retry);
    expect(onRetryIndex).toHaveBeenCalledTimes(1);
  });

  it("says so without a retry when a list filled its page", () => {
    tab({ recordingIndex: "capped", onRetryIndex: () => {} });
    expect(screen.getByText(RECORDING_INDEX_INCOMPLETE.NOTE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: RECORDING_INDEX_INCOMPLETE.RETRY_LABEL })).not.toBeInTheDocument();
  });

  it("says nothing when the index read whole", () => {
    tab({ recordingIndex: "complete" });
    expect(screen.queryByTestId("recording-index-incomplete")).not.toBeInTheDocument();
  });
});
