/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../lib/types";

// MEDIUM fixes pinned here:
//  - run_id filter must query the SERVER with run_id (api.listAudit(runId)),
//    not filter client-side over a truncated global window.
//  - server append (seq) order must be preserved — no client re-sort by time.
//  - a capped (>= 500) global window must show a "truncated" indicator.
//  - the Event facet (kind bucketing) must narrow the loaded window.

const listAuditMock = vi.fn();
const getRunMock = vi.fn();
vi.mock("../../lib/api", () => ({
  api: {
    listAudit: (...a: unknown[]) => listAuditMock(...a),
    getRun: (...a: unknown[]) => getRunMock(...a),
  },
}));

import { AuditScreen } from "./audit";

function ev(partial: Partial<AuditEvent> & { id: string }): AuditEvent {
  return {
    time: "2026-06-28T00:00:00.000Z",
    actor_type: "agent",
    actor: "spiffe://wardyn/agent",
    action: "tool.call",
    outcome: "success",
    ...partial,
  } as AuditEvent;
}

// The screen renders a real react-router <Link> (Open run, in the drill
// banner) — wrap it in a MemoryRouter so that mounts outside the app's real
// BrowserRouter (main.tsx) without throwing.
function renderScreen() {
  return render(
    <MemoryRouter>
      <AuditScreen />
    </MemoryRouter>,
  );
}

describe("AuditScreen", () => {
  beforeEach(() => {
    listAuditMock.mockReset();
    getRunMock.mockReset();
    getRunMock.mockResolvedValue(undefined);
  });

  it("re-queries the server with run_id when a run is selected from a row", async () => {
    // First load: global (no run filter) -> two events for different runs.
    listAuditMock.mockResolvedValueOnce([
      ev({ id: "e1", run_id: "run_111", action: "egress.allow" }),
      ev({ id: "e2", run_id: "run_222", action: "egress.deny" }),
    ]);
    // Second load (after clicking the run drill-in): server returns that run's
    // authoritative trail.
    listAuditMock.mockResolvedValueOnce([ev({ id: "e1", run_id: "run_111", action: "egress.allow" })]);

    renderScreen();
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith(undefined));

    // The run drill-in chip shows the id without the run_ prefix.
    const drill = await screen.findByRole("button", { name: /111/ });
    fireEvent.click(drill);

    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith("run_111"));
  });

  it("preserves the server append order (does not re-sort by wall-clock time)", async () => {
    // Server returns seq order e1 then e2, but e2 has an EARLIER timestamp.
    // A buggy client re-sort by time would flip them; we must keep e1 first.
    listAuditMock.mockResolvedValue([
      ev({ id: "e1", action: "first.action", time: "2026-06-28T00:00:02.000Z" }),
      ev({ id: "e2", action: "second.action", time: "2026-06-28T00:00:01.000Z" }),
    ]);
    renderScreen();

    const first = await screen.findByText("first.action");
    const second = await screen.findByText("second.action");
    // first.action must appear before second.action in DOM order.
    expect(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("shows a truncation indicator when the global window is capped at 500", async () => {
    const many = Array.from({ length: 500 }, (_, i) => ev({ id: `e${i}`, action: `act.${i}` }));
    listAuditMock.mockResolvedValue(many);
    renderScreen();

    await waitFor(() => expect(screen.getByText(/truncated/i)).toBeInTheDocument());
  });

  it("does not show the truncation indicator below the cap", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1" })]);
    renderScreen();

    await screen.findByText(/1 event/);
    expect(screen.queryByText(/truncated/i)).not.toBeInTheDocument();
  });

  // M20: a run_id-filtered query hits a HIGHER server cap (1000, vs 500 for the
  // unfiltered view) — but a long/chatty run's own trail can still hit it. This
  // used to be suppressed unconditionally whenever a run filter was active.
  it("M20: shows the truncation indicator for a run-filtered view capped at 1000", async () => {
    listAuditMock.mockResolvedValueOnce([ev({ id: "e0", run_id: "run_111", action: "egress.allow" })]);
    const many = Array.from({ length: 1000 }, (_, i) =>
      ev({ id: `r${i}`, run_id: "run_111", action: `act.${i}` }),
    );
    listAuditMock.mockResolvedValueOnce(many);
    renderScreen();
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith(undefined));

    fireEvent.click(await screen.findByRole("button", { name: /111/ }));
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith("run_111"));

    await waitFor(() => expect(screen.getByText(/truncated/i)).toBeInTheDocument());
    expect(screen.getByText(/first 1000 events for this run/i)).toBeInTheDocument();
  });

  it("M20: does not show the truncation indicator for a run-filtered view below 1000", async () => {
    listAuditMock.mockResolvedValueOnce([ev({ id: "e0", run_id: "run_111", action: "egress.allow" })]);
    listAuditMock.mockResolvedValueOnce([ev({ id: "e0", run_id: "run_111", action: "egress.allow" })]);
    renderScreen();
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith(undefined));

    fireEvent.click(await screen.findByRole("button", { name: /111/ }));
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith("run_111"));

    await screen.findByText(/1 event/);
    expect(screen.queryByText(/truncated/i)).not.toBeInTheDocument();
  });

  it("filters by event kind via the Event facet select", async () => {
    listAuditMock.mockResolvedValue([
      ev({ id: "e1", action: "run.create" }),
      ev({ id: "e2", action: "egress.deny", target: "evil.example.com", outcome: "denied" }),
    ]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText("Created the run");
    expect(screen.getByText(/Denied egress to evil\.example\.com/)).toBeInTheDocument();

    await user.click(screen.getByRole("combobox", { name: /event/i }));
    await user.click(await screen.findByRole("option", { name: /^Egress$/i }));

    await waitFor(() => expect(screen.queryByText("Created the run")).not.toBeInTheDocument());
    expect(screen.getByText(/Denied egress to evil\.example\.com/)).toBeInTheDocument();
  });
});
