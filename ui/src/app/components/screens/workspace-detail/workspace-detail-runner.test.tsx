/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { Workspace, WorkspaceSourceInput } from "../../../lib/types";
import type { RunnerView } from "../../../lib/types/runners";
import { OperatorProvider } from "../../wardyn/operator-context";

const getWorkspaceMock = vi.fn();
const listMineMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({ workspaces: { getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a) } }));
vi.mock("../../../lib/api/runners", () => ({ runners: { listMine: (...a: unknown[]) => listMineMock(...a) } }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn().mockResolvedValue({ runner: { driver: "docker" } }) } }));
vi.mock("../../../lib/api/runs", () => ({ runs: {} }));
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: { ...actual.permissions, getAvailability: () => new Promise(() => {}), getMyCapabilities: () => new Promise(() => {}) },
  };
});
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { WorkspaceDetailScreen } from "./workspace-detail";

const RUNNER_ID = "abcdef12-3456-7890-abcd-ef1234567890";
const MAC = { id: RUNNER_ID, name: "ec2-mac-rc9", state: "claimed", owner_kind: "person", kind: "self_hosted" } as RunnerView;

function folderWs(source: Partial<WorkspaceSourceInput>): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "local_dir",
    source: "/Users/dana/work/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    sources: [{ type: "local_dir", path: "/Users/dana/work/payments", ...source }],
  };
}

function renderDetail(ws: Workspace) {
  getWorkspaceMock.mockResolvedValue(ws);
  return render(
    <MemoryRouter initialEntries={["/workspaces/ws-1"]}>
      <OperatorProvider operator securityOperator>
        <Routes>
          <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const startRun = () => screen.getByRole("button", { name: "Start a run" });

beforeEach(() => {
  vi.clearAllMocks();
  listMineMock.mockResolvedValue([MAC]);
});

describe("Workspace detail: a folder on a runner", () => {
  it("a claimed runner shows only the 'on <runner>' chip, with no warning and no refusal", async () => {
    renderDetail(folderWs({ runner_id: RUNNER_ID, runner_state: "claimed" }));
    expect(await screen.findByText("on ec2-mac-rc9")).toBeInTheDocument();
    for (const chip of ["Runner revoked", "Runner removed", "Not your runner"]) expect(screen.queryByText(chip)).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(startRun()).toBeEnabled();
  });

  it("a folder on the server shows no runner chip", async () => {
    renderDetail(folderWs({}));
    await screen.findByRole("heading", { name: "payments" });
    expect(screen.queryByText(/^on /)).not.toBeInTheDocument();
  });

  it.each([
    ["revoked", "Runner revoked", "runner revoked"],
    ["not_owner", "Not your runner", "not your runner"],
  ] as const)("a %s runner shows its state chip and the refusal naming the runner", async (state, chip, word) => {
    renderDetail(folderWs({ runner_id: RUNNER_ID, runner_state: state }));
    expect(await screen.findByText(chip)).toBeInTheDocument();
    expect(screen.getByText("on ec2-mac-rc9")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      `The folder for payments is on ec2-mac-rc9, which can't run it now (${word}). Add a new workspace with a folder on a runner you own or on the Wardyn server.`,
    );
    expect(startRun()).toBeDisabled();
  });

  it("a removed runner is named by the first 8 characters of its id", async () => {
    listMineMock.mockResolvedValue([]);
    renderDetail(folderWs({ runner_id: RUNNER_ID, runner_state: "missing" }));
    expect(await screen.findByText("Runner removed")).toBeInTheDocument();
    expect(screen.getByText("on abcdef12")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "The folder for payments is on abcdef12, which can't run it now (runner removed). Add a new workspace with a folder on a runner you own or on the Wardyn server.",
    );
  });
});
