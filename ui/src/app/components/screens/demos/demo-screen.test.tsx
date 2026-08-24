/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

// Mock the api client so createRun/getRun are asserted and no network happens.
const getSetupStatusMock = vi.fn();
const createRunMock = vi.fn();
const getRunMock = vi.fn();
const killRunMock = vi.fn();
const getGrantsMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    createRun: (...a: unknown[]) => createRunMock(...a),
    getRun: (...a: unknown[]) => getRunMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
    getGrants: (...a: unknown[]) => getGrantsMock(...a),
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
  demoAuditRows: () => [],
}));
const toastErrorMock = vi.fn();
vi.mock("sonner", () => ({ toast: { error: (...a: unknown[]) => toastErrorMock(...a) } }));

import { DemoScreen } from "./demo-screen";
import { DEMOS } from "./demo-catalog";
import { baseStatus } from "../../../lib/test-fixtures";
import { HttpError } from "../../../lib/api/core";

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
    getGrantsMock.mockReset().mockResolvedValue([]);
    listAuditMock.mockReset().mockResolvedValue([]);
    toastErrorMock.mockReset();
  });

  // W3-S1-5: a kill that LOSES the server's "state changed concurrently" race
  // does not tear the sandbox down — the run is still live server-side. End
  // demo must not treat that 409 like a clean stop: it must surface the
  // failure and keep tracking the run (UI + localStorage), or the operator is
  // told "ended" while a sandbox keeps running unattended.
  it("a losing-race 409 on End demo keeps the run tracked and toasts an error, instead of orphaning it", async () => {
    renderScreen();
    const startBtn = (await screen.findAllByRole("button", { name: /start demo/i }))[0];
    await user.click(startBtn);
    await screen.findByTestId("attach-terminal");
    expect(JSON.parse(localStorage.getItem("wardyn-demo-runs")!)).toEqual({ [DEMOS[0].id]: "demo-run-1" });

    killRunMock.mockRejectedValueOnce(
      new HttpError(409, "run state changed concurrently; not overwriting with KILLED"),
    );
    await user.click(screen.getByRole("button", { name: /end demo/i }));

    expect(toastErrorMock).toHaveBeenCalledTimes(1);
    // Still tracked: the terminal stays mounted and localStorage still holds
    // the run, so a reload re-attaches instead of losing the only handle to it.
    expect(screen.getByTestId("attach-terminal")).toBeInTheDocument();
    expect(JSON.parse(localStorage.getItem("wardyn-demo-runs")!)).toEqual({ [DEMOS[0].id]: "demo-run-1" });
  });

  // The OTHER 409 shape ("already terminal") means the run genuinely ended on
  // its own — that race is benign and End demo must still clean up quietly (no
  // toast, never re-attach on reload). Unlike before, the run stays tracked
  // in memory rather than being force-forgotten: the next poll tick confirms
  // the real terminal state and the card settles into its terminated view
  // ("Turn this into a policy" / Start again) instead of the live terminal.
  it("an already-terminal 409 on End demo forgets it quietly (no toast); the poll settles the card into the terminated view", async () => {
    renderScreen();
    const startBtn = (await screen.findAllByRole("button", { name: /start demo/i }))[0];
    await user.click(startBtn);
    await screen.findByTestId("attach-terminal");

    killRunMock.mockRejectedValueOnce(new HttpError(409, "run is already terminal (state=KILLED); not re-killing"));
    getRunMock.mockResolvedValue({ id: "demo-run-1", state: "KILLED" });
    await user.click(screen.getByRole("button", { name: /end demo/i }));

    expect(toastErrorMock).not.toHaveBeenCalled();
    expect(localStorage.getItem("wardyn-demo-runs")).toBeNull();
    // The poll ticks every 2s (usePoll's real setInterval) — give it room to fire.
    await waitFor(() => expect(screen.queryByTestId("attach-terminal")).not.toBeInTheDocument(), {
      timeout: 3000,
    });
    expect(await screen.findByTestId("demo-terminated")).toBeInTheDocument();
  });

  it("renders the nine keyless demo cards, and hides the harness demo without a model", async () => {
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

  it("Start on the harness demo posts an interactive run with NO task_mode=exec (it needs the model)", async () => {
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
      task_mode: undefined,
    });
  });

  it("gates Start on barrierReady — disabled + hint on a runner-less host", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ ready: false, runner: { driver: "none", confinement_classes: [] } }),
    );
    renderScreen();
    expect(await screen.findByTestId("demos-not-ready")).toBeInTheDocument();
    const starts = screen.getAllByRole("button", { name: /start demo/i });
    expect(starts).toHaveLength(9);
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
      task_mode: "exec",
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

  // authorized-not-issued's mint command carries a literal "{grant_id}" token —
  // StepList must render it AS-IS before a run exists (the "what you'll run"
  // preview has no run to ask), then substitute the real id once one is live.
  it("StepList renders {grant_id} literally pre-launch, and substitutes it once a run's grant is fetched", async () => {
    getGrantsMock.mockResolvedValue([{ id: "grant-123", scope: "api_key", audience: "api_key", state: "active" }]);
    renderScreen();
    const demo = DEMOS.find((d) => d.id === "authorized-not-issued")!;

    await screen.findByText(demo.title);
    expect(screen.getAllByText(/\{grant_id\}/).length).toBeGreaterThan(0);

    await user.click(screen.getByTestId(`demo-start-${demo.id}`));
    await waitFor(() => expect(screen.getAllByText(/grant-123/).length).toBeGreaterThan(0));
    expect(screen.queryAllByText(/\{grant_id\}/)).toHaveLength(0);
  });
});
