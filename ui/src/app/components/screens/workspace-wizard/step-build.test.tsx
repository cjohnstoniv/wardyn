/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import { StepBuild } from "./step-build";
import type { WorkspaceBuildState } from "../../../lib/api/workspaces";

const buildWorkspaceMock = vi.fn();
const getWorkspaceBuildMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    buildWorkspace: (...a: unknown[]) => buildWorkspaceMock(...a),
    getWorkspaceBuild: (...a: unknown[]) => getWorkspaceBuildMock(...a),
  },
}));

beforeEach(() => {
  buildWorkspaceMock.mockReset();
  getWorkspaceBuildMock.mockReset();
});

describe("StepBuild — the build log pane", () => {
  it("renders the log tail while building", async () => {
    const state: WorkspaceBuildState = {
      state: "building",
      started_at: new Date().toISOString(),
      log: ["pulling envbuilder:latest", "cloning acme/payments@main"],
    };
    buildWorkspaceMock.mockResolvedValue(state);
    render(<StepBuild workspaceId="ws-1" onStateChange={vi.fn()} />);

    const pane = await screen.findByTestId("build-log");
    expect(pane).toHaveTextContent("pulling envbuilder:latest");
    expect(pane).toHaveTextContent("cloning acme/payments@main");
  });

  it("keeps the log visible once the build lands on done — the debugging story stays up", async () => {
    buildWorkspaceMock.mockResolvedValue({
      state: "done",
      image: "wardyn-workspace/ws-1:abc123",
      log: ["Step 1/3 : FROM base", "Successfully built deadbeef"],
    });
    render(<StepBuild workspaceId="ws-1" onStateChange={vi.fn()} />);

    expect(await screen.findByText("Image ready")).toBeInTheDocument();
    const pane = await screen.findByTestId("build-log");
    expect(pane).toHaveTextContent("Successfully built deadbeef");
  });

  it("keeps the log visible on failure alongside the failure line — the log IS the debugging story", async () => {
    buildWorkspaceMock.mockResolvedValue({
      state: "failed",
      detail: "build failed with exit code 1",
      log: ["Step 2/3 : RUN go build ./...", "exit code 1"],
    });
    render(<StepBuild workspaceId="ws-1" onStateChange={vi.fn()} />);

    expect(await screen.findByText("Build failed")).toBeInTheDocument();
    expect(screen.getByText("build failed with exit code 1")).toBeInTheDocument();
    const pane = await screen.findByTestId("build-log");
    expect(pane).toHaveTextContent("exit code 1");
  });

  it("renders no log pane when the response carries none (a cache-hit done, or nothing built yet)", async () => {
    buildWorkspaceMock.mockResolvedValue({ state: "done", image: "wardyn-workspace/ws-1:cached" });
    render(<StepBuild workspaceId="ws-1" onStateChange={vi.fn()} />);

    await screen.findByText("Image ready");
    expect(screen.queryByTestId("build-log")).not.toBeInTheDocument();
  });

  it("a later poll tick's fresh log replaces the tail shown (not just the initial kick's)", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      buildWorkspaceMock.mockResolvedValue({
        state: "building",
        started_at: new Date().toISOString(),
        log: ["step one"],
      });
      getWorkspaceBuildMock.mockResolvedValue({
        state: "building",
        started_at: new Date().toISOString(),
        log: ["step one", "step two", "step three"],
      });

      render(<StepBuild workspaceId="ws-1" onStateChange={vi.fn()} />);
      await waitFor(() => expect(screen.getByTestId("build-log")).toHaveTextContent("step one"));

      await act(() => vi.advanceTimersByTimeAsync(3000));
      await waitFor(() => expect(getWorkspaceBuildMock).toHaveBeenCalled());
      await waitFor(() => expect(screen.getByTestId("build-log")).toHaveTextContent("step three"));
    } finally {
      vi.useRealTimers();
    }
  });
});
