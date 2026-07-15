/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

// Mock the api client so createRun/getRun are asserted and no network happens.
const getSetupStatusMock = vi.fn();
const createRunMock = vi.fn();
const getRunMock = vi.fn();
const killRunMock = vi.fn();
vi.mock("../../../lib/api", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api")>("../../../lib/api");
  return {
    HttpError: actual.HttpError,
    api: {
      getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a),
      createRun: (...a: unknown[]) => createRunMock(...a),
      getRun: (...a: unknown[]) => getRunMock(...a),
      killRun: (...a: unknown[]) => killRunMock(...a),
    },
  };
});

// AttachTerminal drags in xterm + a live WebSocket; LiveApprovals polls the API.
// Stub both to inert markers so the card composition is what's under test.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: ({ runId }: { runId: string }) => <div data-testid="live-approvals">{runId}</div>,
}));

import { DemoScreen } from "./demo-screen";
import { DEMOS } from "./demo-catalog";
import { baseStatus } from "../setup/test-fixtures";

function renderScreen() {
  return render(
    <MemoryRouter>
      <DemoScreen />
    </MemoryRouter>,
  );
}

describe("DemoScreen", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  beforeEach(() => {
    localStorage.clear();
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus({ ready: true }));
    createRunMock.mockReset().mockResolvedValue({ id: "demo-run-1", state: "RUNNING" });
    getRunMock.mockReset().mockResolvedValue({ id: "demo-run-1", state: "RUNNING" });
    killRunMock.mockReset().mockResolvedValue(undefined);
  });

  it("renders all four demo cards", async () => {
    renderScreen();
    for (const d of DEMOS) {
      expect(await screen.findByText(d.title)).toBeInTheDocument();
    }
  });

  it("gates Start on barrierReady — disabled + hint on a runner-less host", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ ready: false, runner: { driver: "none", confinement_classes: [] } }),
    );
    renderScreen();
    expect(await screen.findByTestId("demos-not-ready")).toBeInTheDocument();
    const starts = screen.getAllByRole("button", { name: /start demo/i });
    expect(starts).toHaveLength(4);
    for (const b of starts) expect(b).toBeDisabled();
  });

  it("Start posts { agent, interactive, inline_policy } with the demo's exact policy", async () => {
    renderScreen();
    const first = (await screen.findAllByRole("button", { name: /start demo/i }))[0];
    await user.click(first);
    expect(createRunMock).toHaveBeenCalledWith({
      agent: "claude-code",
      interactive: true,
      inline_policy: DEMOS[0].policy,
    });
  });

  it("an active demo shows the terminal, live approvals, and End demo", async () => {
    renderScreen();
    const first = (await screen.findAllByRole("button", { name: /start demo/i }))[0];
    await user.click(first);
    expect(await screen.findByTestId("attach-terminal")).toHaveTextContent("demo-run-1");
    expect(screen.getByTestId("live-approvals")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /end demo/i })).toBeInTheDocument();
  });
});
