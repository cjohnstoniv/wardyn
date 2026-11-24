/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workspace detail keeps today's states through TanStack Query: a new id never shows the last workspace or its error,
// a failure keeps its own sentence, a background failure keeps the workspace, Retry shows loading until it answers,
// and a write's answer shows with no second read.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import type { Workspace } from "../../../lib/types";
import { HttpError } from "../../../lib/api/core";
import { STATES } from "../../wardyn/states";
import { OperatorProvider } from "../../wardyn/operator-context";

const getWorkspaceMock = vi.fn();
const setApprovedEgressMock = vi.fn();
const setRequirementsMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    setApprovedEgress: (...a: unknown[]) => setApprovedEgressMock(...a),
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
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

import { WorkspaceDetailScreen } from "./workspace-detail";

const ws = (over: Partial<Workspace> = {}): Workspace => ({
  id: "ws-a",
  name: "alpha",
  kind: "local_dir",
  source: "/Users/dana/work/alpha",
  status: "scanned",
  created_at: "",
  updated_at: "rev-1",
  ...over,
});
const A = ws();
const B = ws({ id: "ws-b", name: "bravo" });

let go: (to: string) => void = () => {};
function Nav() {
  const navigate = useNavigate();
  go = (to) => navigate(to);
  return null;
}
const renderAt = (id: string) =>
  render(
    <MemoryRouter initialEntries={[`/workspaces/${id}`]}>
      <OperatorProvider operator securityOperator>
        <Nav />
        <Routes>
          <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );

const settle = () => act(async () => void (await new Promise((resolve) => setTimeout(resolve, 20))));
const heading = (name: string) => screen.findByRole("heading", { level: 1, name });
const hung = <T,>() => {
  let resolve: (v: T) => void = () => {};
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
};
const isSkeleton = () => document.querySelector(".animate-pulse") !== null;

beforeEach(() => {
  getWorkspaceMock.mockReset();
  setApprovedEgressMock.mockReset();
  setRequirementsMock.mockReset();
});
afterEach(() => cleanup());

describe("Workspace detail failure hold", () => {
  it("navigating from one id to another never shows the first workspace", async () => {
    const answer = hung<Workspace>();
    getWorkspaceMock.mockImplementation((id: string) => (id === "ws-a" ? Promise.resolve(A) : answer.promise));
    renderAt("ws-a");
    await heading("alpha");
    act(() => go("/workspaces/ws-b"));
    await settle();
    expect(screen.queryByRole("heading", { level: 1, name: "alpha" })).not.toBeInTheDocument();
    expect(isSkeleton()).toBe(true);
    await act(async () => answer.resolve(B));
    await heading("bravo");
  });

  it("a failure on A, then navigating to B, shows loading and then B, never A's error", async () => {
    const answer = hung<Workspace>();
    getWorkspaceMock.mockImplementation((id: string) => (id === "ws-a" ? Promise.reject(new HttpError(500, "alpha is down", "load_failed")) : answer.promise));
    renderAt("ws-a");
    expect(await screen.findByText("alpha is down")).toBeInTheDocument();
    act(() => go("/workspaces/ws-b"));
    await settle();
    expect(screen.queryByText("alpha is down")).not.toBeInTheDocument();
    expect(isSkeleton()).toBe(true);
    await act(async () => answer.resolve(B));
    await heading("bravo");
    expect(screen.queryByText("alpha is down")).not.toBeInTheDocument();
  });

  it("A fails, B's read hangs, then back to A shows loading while A's new read is pending, never A's error", async () => {
    const again = hung<Workspace>();
    getWorkspaceMock.mockImplementation((id: string) => (id === "ws-a" ? Promise.reject(new HttpError(500, "alpha is down", "load_failed")) : new Promise(() => {})));
    renderAt("ws-a");
    expect(await screen.findByText("alpha is down")).toBeInTheDocument();
    act(() => go("/workspaces/ws-b"));
    await settle();
    getWorkspaceMock.mockImplementation((id: string) => (id === "ws-a" ? again.promise : new Promise(() => {})));
    act(() => go("/workspaces/ws-a"));
    await settle();
    expect(screen.queryByText("alpha is down")).not.toBeInTheDocument();
    expect(isSkeleton()).toBe(true);
    await act(async () => again.resolve(A));
    await heading("alpha");
  });

  it("a background failure keeps the workspace", async () => {
    // A recording session polls the workspace; its failed poll must leave the page as it was.
    const recording = ws({ record_results: { build: { run_id: "run-1", mode: "auto", status: "recording" } } });
    getWorkspaceMock.mockResolvedValueOnce(recording).mockRejectedValueOnce(new HttpError(500, "boom"));
    renderAt("ws-a");
    await heading("alpha");
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await settle();
    expect(getWorkspaceMock).toHaveBeenCalledTimes(2);
    await heading("alpha");
    expect(screen.queryByText(STATES.ERROR_DEFAULT)).not.toBeInTheDocument();
  });

  it("Retry after an error shows loading, then the workspace", async () => {
    getWorkspaceMock.mockRejectedValueOnce(new HttpError(500, "alpha is down", "load_failed"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderAt("ws-a");
    expect(await screen.findByText("alpha is down")).toBeInTheDocument();
    const answer = hung<Workspace>();
    getWorkspaceMock.mockReturnValueOnce(answer.promise);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await settle();
    expect(screen.queryByText("alpha is down")).not.toBeInTheDocument();
    expect(isSkeleton()).toBe(true);
    await act(async () => answer.resolve(A));
    await heading("alpha");
  });

  it("a failed read, then a Retry answered 404, reaches the not-found page", async () => {
    getWorkspaceMock.mockRejectedValueOnce(new HttpError(500, "alpha is down", "load_failed")).mockResolvedValueOnce(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderAt("ws-a");
    await user.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByText(/workspace not found/i)).toBeInTheDocument();
  });

  it.each([
    [403, "you may not read this workspace", "forbidden"],
    [500, "the server could not read it", "load_failed"],
  ])("a %i shows its own sentence in the error state, not the not-found page", async (status, message, reason) => {
    getWorkspaceMock.mockRejectedValueOnce(new HttpError(status, message, reason));
    renderAt("ws-a");
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByText(/workspace not found/i)).not.toBeInTheDocument();
  });

  it("a write's answer shows with no second getWorkspace call", async () => {
    const hosted = ws({ approved_egress: ["registry.npmjs.org"], requirements: { "egress:registry.npmjs.org": { level: "required", provenance: "operator_set" } } });
    getWorkspaceMock.mockResolvedValue(hosted);
    setApprovedEgressMock.mockResolvedValue({ ...hosted, approved_egress: [] });
    setRequirementsMock.mockResolvedValue(ws({ approved_egress: [], requirements: {} }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderAt("ws-a");
    expect(await screen.findByText("Allowed hosts · 1")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove registry.npmjs.org" }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: /remove/i }));
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Allowed hosts · 0")).toBeInTheDocument();
    expect(getWorkspaceMock).toHaveBeenCalledTimes(1);
  });
});
