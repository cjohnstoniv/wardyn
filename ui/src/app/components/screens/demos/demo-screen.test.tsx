/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

// Mock the api client so createRun/getRun are asserted and no network happens.
const getSetupStatusMock = vi.fn();
const createRunMock = vi.fn();
const getRunMock = vi.fn();
const killRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    createRun: (...a: unknown[]) => createRunMock(...a),
    getRun: (...a: unknown[]) => getRunMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
  },
}));
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

// AttachTerminal drags in xterm + a live WebSocket; LiveApprovals polls the API.
// Stub both to inert markers so the card composition is what's under test.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: ({ runId }: { runId: string }) => <div data-testid="live-approvals">{runId}</div>,
}));
// The inline audit panel polls /audit; stub it to no decisions (empty projection).
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
}));

import { DemoScreen, demoRunBody } from "./demo-screen";
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
    listAuditMock.mockReset().mockResolvedValue([]);
  });

  it("renders the four keyless demo cards, and hides the harness demo without a model", async () => {
    renderScreen();
    for (const d of DEMOS.filter((d) => !d.needsModel)) {
      expect(await screen.findByText(d.title)).toBeInTheDocument();
    }
    const harness = DEMOS.find((d) => d.needsModel)!;
    expect(screen.queryByText(harness.title)).not.toBeInTheDocument();
  });

  it("shows the harness demo card, with Start enabled, once a model is connected", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        ready: true,
        providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
      }),
    );
    renderScreen();
    const harness = DEMOS.find((d) => d.needsModel)!;
    expect(await screen.findByText(harness.title)).toBeInTheDocument();
    expect(screen.getByTestId(`demo-start-${harness.id}`)).toBeEnabled();
  });

  it("Start on the harness demo posts an interactive run (the operator drives the agent)", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        ready: true,
        providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
      }),
    );
    renderScreen();
    const harness = DEMOS.find((d) => d.needsModel)!;
    const start = await screen.findByTestId(`demo-start-${harness.id}`);
    await user.click(start);
    expect(createRunMock).toHaveBeenCalledWith({
      agent: "claude-code",
      interactive: true,
      inline_policy: harness.policy,
    });
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

  it("an active demo shows the terminal, live approvals, the inline audit panel, and End demo", async () => {
    renderScreen();
    const first = (await screen.findAllByRole("button", { name: /start demo/i }))[0];
    await user.click(first);
    expect(await screen.findByTestId("attach-terminal")).toHaveTextContent("demo-run-1");
    expect(screen.getByTestId("live-approvals")).toBeInTheDocument();
    expect(screen.getByTestId("demo-audit-panel")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /end demo/i })).toBeInTheDocument();
  });
});

// demoRunBody — the shared body-builder Getting-Started's harness step preflights
// and launches through (D1's overlay). No overlay: identical to the pre-overlay
// literal every other demo still launches with (the two tests above pin that
// exact shape) — proven here directly so a future edit to either can't drift.
describe("demoRunBody", () => {
  it("omits agent/integration_id overrides with no overlay — the unchanged keyless shape", () => {
    expect(demoRunBody(DEMOS[0])).toEqual({
      agent: "claude-code",
      interactive: true,
      inline_policy: DEMOS[0].policy,
    });
  });

  it("folds in an explicit integration_id and switches the agent for an OpenAI overlay", () => {
    const harness = DEMOS.find((d) => d.needsModel)!;
    expect(demoRunBody(harness, { agent: "codex-cli", integration_id: "ai:openai_api_key" })).toEqual({
      agent: "codex-cli",
      interactive: true,
      inline_policy: harness.policy,
      integration_id: "ai:openai_api_key",
    });
  });
});
