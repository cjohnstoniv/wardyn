/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1476 review F1: when /me fails, `operator` fails open as TRUE while the
// principal is a placeholder, so the entry rule (the run's person, or an admin on
// an operator-owned run) reads false for the run's REAL owner. The page must not
// take the terminal, the SSH card or the tile from them on a guess: an unresolved
// identity defers to the server, whose ticket mint is the enforcement point.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
    getFiles: vi.fn().mockRejectedValue(new Error("no runner")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner")),
    getAttachHolder: vi.fn().mockResolvedValue({ held: false }),
    takeoverAttach: vi.fn(),
    attachTicket: vi.fn(),
  },
}));
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: vi.fn().mockResolvedValue([]), approve: vi.fn(), deny: vi.fn() },
}));
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../lib/api/audit")>()),
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
}));
vi.mock("../../lib/api/recordings", () => ({ recordings: { getRecording: vi.fn().mockResolvedValue(null) } }));
vi.mock("../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({ status: "ok" }) } }));
vi.mock("../../lib/api/ssh-keys", () => ({ sshKeys: { listKeys: vi.fn().mockResolvedValue([]) } }));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn(), success: vi.fn() } }));
// The real terminal needs xterm; what matters here is whether the page MOUNTS it.
vi.mock("../attach-terminal", () => ({ AttachTerminal: () => <div data-testid="attach-terminal" /> }));

import { RunDetailScreen } from "./run-detail";
import { OperatorProvider } from "../wardyn/operator-context";
import { RUN_WIDGETS } from "./run-detail/widget-registry";
import { ConnectSSHCard } from "./run-detail-ssh";
import type { RunDetail } from "../../lib/types";

const RUN = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "alice@acme.io",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: true,
};

// Exactly app-shell's post-/me-failure state: fail-open operator, the "unknown"
// principal, and a run owned by a real person.
function renderUnresolved() {
  getRunMock.mockResolvedValue(RUN);
  return render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <OperatorProvider operator={true} operatorResolved={false} principal="unknown">
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  getRunMock.mockReset();
});

describe("an unresolved /me defers entry to the server", () => {
  it("the run page still mounts the terminal, and claims no refusal and no owner line", async () => {
    renderUnresolved();
    expect(await screen.findByTestId("attach-terminal")).toBeInTheDocument();
    expect(screen.queryByText(/can open this run's terminal/)).not.toBeInTheDocument();
    expect(screen.getByText("Interactive — attachable")).toBeInTheDocument();
  });

  it("the SSH card shows", async () => {
    render(
      <MemoryRouter>
        <OperatorProvider operator={true} operatorResolved={false} principal="unknown">
          <ConnectSSHCard run={RUN as unknown as RunDetail} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Attach from your terminal")).toBeInTheDocument());
  });

  it("the SSH tile is available", () => {
    expect(
      RUN_WIDGETS.ssh.available?.({
        run: RUN as unknown as RunDetail,
        principal: "unknown",
        operator: true,
        operatorResolved: false,
        view: "user",
      } as never),
    ).toBe(true);
  });

  it("a RESOLVED admin on that person's run is still refused, so the arm is only for the unknown", async () => {
    getRunMock.mockResolvedValue(RUN);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={true} operatorResolved principal="sam@acme.io">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText("Only alice@acme.io can open this run's terminal, apps and SSH.")).toBeInTheDocument();
    expect(screen.queryByTestId("attach-terminal")).not.toBeInTheDocument();
  });
});
