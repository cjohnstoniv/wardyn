/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wizard's live verify session: launch is CONFINED (held-at-the-door is
// the whole point), a refused start renders the server's reason inline, Done
// verifying kills the run then tells the wizard to absorb the rows the
// approve hook wrote server-side, and a session already running server-side
// is picked back up on mount instead of orphaned (UI-WS-10).
import { useState } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Workspace } from "../../../lib/types";

const recordTaskMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { recordTask: (...a: unknown[]) => recordTaskMock(...a) },
}));
const killRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { killRun: (...a: unknown[]) => killRunMock(...a) },
}));
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: ({ runId }: { runId: string }) => <div data-testid="live-approvals">{runId}</div>,
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { WizardVerifySession } from "./verify-session";

const ws = { id: "ws-1", name: "pay" } as unknown as Workspace;

// Lifts runId the same way wizard.tsx now does (UI-WS-10) — the component
// itself no longer owns it, so a test exercising a real launch/finish round
// trip needs a stand-in parent to hold it across re-renders.
function Harness({
  ws: harnessWs,
  nothingResolves = false,
  onContractChanged = () => {},
}: {
  ws: Workspace;
  nothingResolves?: boolean;
  onContractChanged?: () => void;
}) {
  const [runId, setRunId] = useState<string | null>(null);
  return (
    <WizardVerifySession
      ws={harnessWs}
      nothingResolves={nothingResolves}
      runId={runId}
      onRunIdChange={setRunId}
      onContractChanged={onContractChanged}
    />
  );
}

beforeEach(() => {
  recordTaskMock.mockReset();
  killRunMock.mockReset().mockResolvedValue(undefined);
});

describe("WizardVerifySession", () => {
  it("launches CONFINED, embeds the terminal + live approvals, and Done kills then refreshes the contract", async () => {
    recordTaskMock.mockResolvedValue({ ok: true, status: 202, record_run_id: "run-9" });
    const onContractChanged = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<Harness ws={ws} nothingResolves={false} onContractChanged={onContractChanged} />);

    await user.click(screen.getByRole("button", { name: "Verify with a session" }));
    await waitFor(() => expect(recordTaskMock).toHaveBeenCalledWith("ws-1", "verify", true));
    expect(await screen.findByTestId("terminal")).toHaveTextContent("run-9");
    expect(screen.getByTestId("live-approvals")).toHaveTextContent("run-9");

    await user.click(screen.getByRole("button", { name: /done verifying/i }));
    await waitFor(() => expect(killRunMock).toHaveBeenCalledWith("run-9"));
    expect(onContractChanged).toHaveBeenCalledTimes(1);
    expect(await screen.findByText(/anything you approved is in the contract now/i)).toBeInTheDocument();
  });

  // ui-wsWizard-1: "Verify with a session" and "Verify in a terminal" used to
  // both render at once when nothingResolves is false, wired to the exact
  // same launch() — two labels claiming to be different actions. Only one
  // launch control may exist.
  it("renders exactly one launch button, never both labels at once", () => {
    render(<Harness ws={ws} nothingResolves={false} />);
    expect(screen.getAllByRole("button", { name: /^verify (with a session|in a terminal)$/i })).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Verify with a session" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Verify in a terminal" })).not.toBeInTheDocument();
  });

  it("renders a refused start (409/503/422) inline with the server's reason", async () => {
    recordTaskMock.mockResolvedValue({ ok: false, status: 409, detail: "an import step is already running" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<Harness ws={ws} nothingResolves onContractChanged={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: "Verify in a terminal" }));
    expect(await screen.findByText(/an import step is already running/i)).toBeInTheDocument();
    expect(screen.queryByTestId("terminal")).not.toBeInTheDocument();
  });
});

// UI-WS-10: this component only mounts while the wizard is on the Verify
// step — a Back/rail click away and back (or a fresh "Edit workspace…" open,
// which is a full remount) used to land back on the launch buttons even
// though the session was still running server-side, and re-launching 409'd.
describe("WizardVerifySession — rehydrates an already-running session", () => {
  it("reattaches on mount from ws.record_results['verify:verify'] when its status is 'recording'", async () => {
    const recordingWs = {
      ...ws,
      record_results: {
        "verify:verify": { run_id: "run-live", label: "verify", mode: "interactive", confined: true, status: "recording" },
      },
    } as unknown as Workspace;

    render(<Harness ws={recordingWs} />);

    expect(await screen.findByTestId("terminal")).toHaveTextContent("run-live");
    expect(screen.getByTestId("live-approvals")).toHaveTextContent("run-live");
    // Reattached, not relaunched.
    expect(recordTaskMock).not.toHaveBeenCalled();
  });

  it("does not reattach to a settled (non-recording) result — the launch buttons stay", async () => {
    const settledWs = {
      ...ws,
      record_results: {
        "verify:verify": { run_id: "run-old", label: "verify", mode: "interactive", confined: true, status: "recorded" },
      },
    } as unknown as Workspace;

    render(<Harness ws={settledWs} />);

    expect(await screen.findByRole("button", { name: "Verify with a session" })).toBeInTheDocument();
    expect(screen.queryByTestId("terminal")).not.toBeInTheDocument();
  });
});
