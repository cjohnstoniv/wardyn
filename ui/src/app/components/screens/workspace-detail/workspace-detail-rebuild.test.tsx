/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A rebuild the server answers "building" is followed until it ends. The page says one is in progress (a second
// click gets the same answer, so it is not a second start), reports how it ended, and tells a status it cannot
// read apart from a build that failed.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";

const getWorkspaceMock = vi.fn();
const buildWorkspaceMock = vi.fn();
const getWorkspaceBuildMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    buildWorkspace: (...a: unknown[]) => buildWorkspaceMock(...a),
    getWorkspaceBuild: (...a: unknown[]) => getWorkspaceBuildMock(...a),
  },
}));
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
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn(), warning: vi.fn() } }));
vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../../../lib/api/approvals", () => ({ approvals: { listApprovals: () => Promise.resolve([]) } }));

import { toast } from "sonner";
import { WorkspaceDetailScreen } from "./workspace-detail";

const workspace = {
  id: "ws-1",
  name: "payments",
  kind: "repo",
  source: "acme/payments",
  ref: "main",
  status: "scanned",
  created_at: "",
  updated_at: "",
  profile: { has_devcontainer: true },
} as unknown as Workspace;

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  getWorkspaceMock.mockResolvedValue(workspace);
  buildWorkspaceMock.mockResolvedValue({ state: "building" });
});
afterEach(() => vi.useRealTimers());

const tick = (ms: number) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)));

async function startRebuild() {
  render(
    <MemoryRouter initialEntries={["/workspaces/ws-1"]}>
      <OperatorProvider operator securityOperator>
        <Routes>
          <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
  await tick(0);
  fireEvent.click(screen.getByRole("button", { name: "Rebuild" }));
  await tick(0);
}

describe("Workspace detail: following a rebuild", () => {
  it("says it is in progress, keeps Rebuild off, and reports the end of the build it followed", async () => {
    getWorkspaceBuildMock.mockResolvedValueOnce({ state: "building" }).mockResolvedValue({ state: "done", image: "img" });
    await startRebuild();
    expect(toast.info).toHaveBeenCalledWith('Rebuild in progress for "payments"');
    expect(screen.getByRole("status")).toHaveTextContent("Rebuild in progress");
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeDisabled();
    await tick(2_500);
    expect(getWorkspaceBuildMock).toHaveBeenCalledWith("ws-1");
    expect(toast.success).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeDisabled();
    await tick(2_500);
    expect(toast.success).toHaveBeenCalledWith('Rebuilt "payments"');
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeEnabled();
  });

  it("a build that failed says so with the server's reason", async () => {
    getWorkspaceBuildMock.mockResolvedValue({ state: "failed", detail: "pull access denied" });
    await startRebuild();
    await tick(2_500);
    expect(toast.error).toHaveBeenCalledWith('Rebuild of "payments" failed', { description: "pull access denied" });
    expect(toast.success).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeEnabled();
  });

  it("a status that cannot be read is not a failed build: it says so, keeps following, and recovers", async () => {
    getWorkspaceBuildMock.mockRejectedValueOnce(new Error("offline")).mockResolvedValue({ state: "done" });
    await startRebuild();
    await tick(2_500);
    expect(screen.getByRole("status")).toHaveTextContent("status cannot be read");
    expect(toast.error).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeDisabled();
    await tick(2_500);
    expect(toast.success).toHaveBeenCalledWith('Rebuilt "payments"');
    expect(screen.queryByRole("status")).toBeNull();
  });
});
