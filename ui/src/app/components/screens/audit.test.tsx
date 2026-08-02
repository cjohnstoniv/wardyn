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
//  - a capped (>= LIST_LIMIT) window must show a "truncated" indicator — and a
//    smaller page must NOT (both views ask for, and get, the server's 1000 max).
//  - the Event facet (kind bucketing) must narrow the loaded window.

const listAuditMock = vi.fn();
const getRunMock = vi.fn();
vi.mock("../../lib/api/audit", () => ({
  audit: {
    listAudit: (...a: unknown[]) => listAuditMock(...a),
  },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
  },
}));
const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: {
    health: () => healthMock(),
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

// 15s suite default, not vitest's 5s: the truncation tests render 1000-event
// feeds through the day-grouping pipeline — ~1-2s alone, but they sit right at
// the 5s ceiling when the full suite runs parallel on a loaded box (same
// rationale as setup-screen.test.tsx's suite timeout).
describe("AuditScreen", { timeout: 15_000 }, () => {
  beforeEach(() => {
    listAuditMock.mockReset();
    getRunMock.mockReset();
    getRunMock.mockResolvedValue(undefined);
    healthMock.mockReset();
    healthMock.mockResolvedValue({});
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

  it("shows a truncation indicator when the global window is capped at 1000", async () => {
    const many = Array.from({ length: 1000 }, (_, i) => ev({ id: `e${i}`, action: `act.${i}` }));
    listAuditMock.mockResolvedValue(many);
    renderScreen();

    await waitFor(() => expect(screen.getByText(/truncated/i)).toBeInTheDocument());
  });

  it("does not cry truncation at the old 500 cap the console no longer asks for", async () => {
    // listAudit sends ?limit=1000 (LIST_LIMIT) and the server honors it, so a
    // 500-event page is COMPLETE — the old 500 threshold said "showing the first
    // 500" for any count from 500 to 999.
    listAuditMock.mockResolvedValue(
      Array.from({ length: 500 }, (_, i) => ev({ id: `e${i}`, action: `act.${i}` })),
    );
    renderScreen();

    await screen.findByText(/500 events/);
    expect(screen.queryByText(/truncated/i)).not.toBeInTheDocument();
  });

  it("does not show the truncation indicator below the cap", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1" })]);
    renderScreen();

    await screen.findByText(/1 event/);
    expect(screen.queryByText(/truncated/i)).not.toBeInTheDocument();
  });

  // a run_id-filtered query gets that run's OWN page (not a slice of the global
  // one) — but a long/chatty run's trail can still hit the cap. This used to be
  // suppressed unconditionally whenever a run filter was active.
  it("shows the truncation indicator for a run-filtered view capped at 1000", async () => {
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

  it("does not show the truncation indicator for a run-filtered view below 1000", async () => {
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

  // A dead or missing kernel sensor is the ABSENCE of events, so the list can
  // never show it — only this chip can. Nothing is claimed when the daemon
  // doesn't report the field at all.
  it("surfaces the eBPF ground-truth state, with its reason, and claims nothing without it", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1" })]);
    healthMock.mockResolvedValue({
      ebpf_groundtruth: { state: "degraded", reason: "heartbeat stale" },
    });
    renderScreen();

    const chip = await screen.findByText(/ground truth · degraded/i);
    expect(chip).toHaveAttribute("title", "heartbeat stale");

    healthMock.mockResolvedValue({});
    renderScreen();
    await waitFor(() => expect(screen.getAllByText(/ground truth/i)).toHaveLength(1));
  });
});
