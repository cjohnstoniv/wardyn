/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wizard's live verify session: launch is CONFINED (held-at-the-door is
// the whole point), a refused start renders the server's reason inline, and
// Done verifying kills the run then tells the wizard to absorb the rows the
// approve hook wrote server-side.
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

beforeEach(() => {
  recordTaskMock.mockReset();
  killRunMock.mockReset().mockResolvedValue(undefined);
});

describe("WizardVerifySession", () => {
  it("launches CONFINED, embeds the terminal + live approvals, and Done kills then refreshes the contract", async () => {
    recordTaskMock.mockResolvedValue({ ok: true, status: 202, record_run_id: "run-9" });
    const onContractChanged = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<WizardVerifySession ws={ws} nothingResolves={false} onContractChanged={onContractChanged} />);

    await user.click(screen.getByRole("button", { name: "Verify with a session" }));
    await waitFor(() => expect(recordTaskMock).toHaveBeenCalledWith("ws-1", "verify", true));
    expect(await screen.findByTestId("terminal")).toHaveTextContent("run-9");
    expect(screen.getByTestId("live-approvals")).toHaveTextContent("run-9");

    await user.click(screen.getByRole("button", { name: /done verifying/i }));
    await waitFor(() => expect(killRunMock).toHaveBeenCalledWith("run-9"));
    expect(onContractChanged).toHaveBeenCalledTimes(1);
    expect(await screen.findByText(/anything you approved is in the contract now/i)).toBeInTheDocument();
  });

  it("renders a refused start (409/503/422) inline with the server's reason", async () => {
    recordTaskMock.mockResolvedValue({ ok: false, status: 409, detail: "an import step is already running" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<WizardVerifySession ws={ws} nothingResolves onContractChanged={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: "Verify in a terminal" }));
    expect(await screen.findByText(/an import step is already running/i)).toBeInTheDocument();
    expect(screen.queryByTestId("terminal")).not.toBeInTheDocument();
  });
});
