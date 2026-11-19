/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// While a session is recording the page polls the workspace every 2.5 s. A server that answers slower than that must
// still move the page on: each read is waited for, not overtaken by the next tick and then discarded.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { RecordResult, Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";

const getWorkspaceMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({ workspaces: { getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a) } }));
vi.mock("../../../lib/api/runners", () => ({ runners: { listMine: () => Promise.resolve([]) } }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: () => new Promise(() => {}) } }));
vi.mock("../../../lib/api/runs", () => ({ runs: {} }));
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: { ...actual.permissions, getAvailability: () => new Promise(() => {}), getMyCapabilities: () => new Promise(() => {}) },
  };
});
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));
// The recording's terminal (xterm) is not what this pins.
vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../../../lib/api/approvals", () => ({ approvals: { listApprovals: () => Promise.resolve([]) } }));

import { WorkspaceDetailScreen } from "./workspace-detail";

const session = (status: RecordResult["status"]): Record<string, RecordResult> => ({ build: { run_id: "run-1", mode: "auto", status } });
const ws = (status: RecordResult["status"]): Workspace => ({
  id: "ws-1",
  name: "payments",
  kind: "local_dir",
  source: "/Users/dana/work/payments",
  status: "scanned",
  created_at: "",
  updated_at: "",
  record_results: session(status),
});

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("Workspace detail: the recording poll under a slow server", () => {
  it("waits for a read slower than the interval, so its answer lands and the settled page stops polling", async () => {
    // The first read shows the session recording; every later one answers 3 s after it was asked, with it recorded.
    getWorkspaceMock
      .mockResolvedValueOnce(ws("recording"))
      .mockImplementation(() => new Promise((done) => setTimeout(() => done(ws("recorded")), 3_000)));
    render(
      <MemoryRouter initialEntries={["/workspaces/ws-1"]}>
        <OperatorProvider operator securityOperator>
          <Routes>
            <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(getWorkspaceMock).toHaveBeenCalledTimes(1);
    // Short steps, so React commits each answer soon after it lands.
    for (let i = 0; i < 40; i++) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(500);
      });
    }
    // One poll read, waited for; its answer settled the session, so the poll paused. A discarded answer would keep it going.
    expect(getWorkspaceMock).toHaveBeenCalledTimes(2);
  });
});
