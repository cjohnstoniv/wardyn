/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F2 (#580, PR #1317 review) — the admin "Older limits" chip + bulk
// "Restart with current limits", over GET /admin/runs/proxy-window and
// POST /admin/runs/restart (RL-10, #575's own server, already merged).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AdminOlderLimitsCard } from "./admin-older-limits-card";

const getAdminProxyWindowMock = vi.fn();
const restartAdminRunsMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getAdminProxyWindow: (...a: unknown[]) => getAdminProxyWindowMock(...a),
    restartAdminRuns: (...a: unknown[]) => restartAdminRunsMock(...a),
  },
}));
const toastErrorMock = vi.fn();
vi.mock("sonner", () => ({ toast: { error: (...a: unknown[]) => toastErrorMock(...a) } }));

function renderCard(operator = true) {
  return render(
    <OperatorProvider operator={operator}>
      <AdminOlderLimitsCard />
    </OperatorProvider>,
  );
}

beforeEach(() => vi.clearAllMocks());

describe("AdminOlderLimitsCard", () => {
  it("renders nothing for a non-operator", async () => {
    getAdminProxyWindowMock.mockResolvedValue({ release: "0.8.0", window: ["0.8"], outside: [{ run_id: "r1", created_by: "u", state: "RUNNING", proxy_release: "0.6" }] });
    const { container } = renderCard(false);
    await new Promise((r) => setTimeout(r, 0));
    expect(container).toBeEmptyDOMElement();
    expect(getAdminProxyWindowMock).not.toHaveBeenCalled();
  });

  it("renders nothing once nothing is outside the window", async () => {
    getAdminProxyWindowMock.mockResolvedValue({ release: "0.8.0", window: ["0.8"], outside: [] });
    const { container } = renderCard();
    await waitFor(() => expect(getAdminProxyWindowMock).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the Older limits chip with the real count, and the canon hint", async () => {
    getAdminProxyWindowMock.mockResolvedValue({
      release: "0.8.0",
      window: ["0.8", "0.7"],
      outside: [
        { run_id: "r1", created_by: "u1", state: "RUNNING", proxy_release: "0.6" },
        { run_id: "r2", created_by: "u2", state: "RUNNING", proxy_release: "0.6" },
      ],
    });
    renderCard();
    expect(await screen.findByText("Older limits · 2")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restart with current limits" })).toBeInTheDocument();
    expect(
      screen.getByText("Restarts each run's network proxy with its owner's current limits. Terminals stay open; requests in flight fail once."),
    ).toBeInTheDocument();
  });

  it("Restart posts exactly the outside ids, renders per-run results, and re-lists", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    getAdminProxyWindowMock.mockResolvedValue({
      release: "0.8.0",
      window: ["0.8"],
      outside: [{ run_id: "r1", created_by: "u1", state: "RUNNING", proxy_release: "0.6" }],
    });
    restartAdminRunsMock.mockResolvedValue({ results: [{ run_id: "r1", ok: true }] });
    renderCard();
    await screen.findByText("Older limits · 1");
    await user.click(screen.getByRole("button", { name: "Restart with current limits" }));
    await waitFor(() => expect(restartAdminRunsMock).toHaveBeenCalledWith(["r1"]));
    expect(await screen.findByText(/Restarted/)).toBeInTheDocument();
    // Re-lists after a restart (best-effort refresh).
    await waitFor(() => expect(getAdminProxyWindowMock).toHaveBeenCalledTimes(2));
  });

  // R2-4 (PR #1317 round-2 review): a real restart rewrites the run's own
  // proxy_release (store_run_revive.go), so the very NEXT listing this same
  // reload asks for comes back with nothing outside the window — the result
  // must survive that, not vanish the instant the card re-lists.
  it("R2-4: keeps the restart result visible after the refetch comes back with nothing outside", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    getAdminProxyWindowMock
      .mockResolvedValueOnce({
        release: "0.8.0",
        window: ["0.8"],
        outside: [{ run_id: "r1", created_by: "u1", state: "RUNNING", proxy_release: "0.6" }],
      })
      .mockResolvedValueOnce({ release: "0.8.0", window: ["0.8"], outside: [] });
    restartAdminRunsMock.mockResolvedValue({ results: [{ run_id: "r1", ok: true }] });
    renderCard();
    await screen.findByText("Older limits · 1");
    await user.click(screen.getByRole("button", { name: "Restart with current limits" }));
    await waitFor(() => expect(getAdminProxyWindowMock).toHaveBeenCalledTimes(2));
    // The chip and button are gone (nothing left outside), but the result of
    // what just happened is still on screen.
    expect(screen.queryByText(/Older limits/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restart with current limits" })).not.toBeInTheDocument();
    expect(await screen.findByText(/Restarted/)).toBeInTheDocument();
  });

  // R2-4, also: reviveBulkMax (run_revive.go) 400s a single request over 100
  // ids — a fleet with more out-of-window runs than that must go in batches.
  it("R2-4: batches a restart above the server's 100-id limit", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const ids = Array.from({ length: 150 }, (_, i) => `r${i}`);
    getAdminProxyWindowMock.mockResolvedValue({
      release: "0.8.0",
      window: ["0.8"],
      outside: ids.map((id) => ({ run_id: id, created_by: "u1", state: "RUNNING", proxy_release: "0.6" })),
    });
    restartAdminRunsMock.mockImplementation(async (batch: string[]) => ({
      results: batch.map((run_id) => ({ run_id, ok: true })),
    }));
    renderCard();
    await screen.findByText("Older limits · 150");
    await user.click(screen.getByRole("button", { name: "Restart with current limits" }));
    await waitFor(() => expect(restartAdminRunsMock).toHaveBeenCalledTimes(2));
    expect(restartAdminRunsMock.mock.calls[0][0]).toHaveLength(100);
    expect(restartAdminRunsMock.mock.calls[1][0]).toHaveLength(50);
  });

  it("a failed result shows the server's OWN refusal text, with no invented 'still lost' gloss on it", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const serverError = "run was lost to a reboot and its agent is stopped; revive it from the run's page";
    getAdminProxyWindowMock.mockResolvedValue({
      release: "0.8.0",
      window: ["0.8"],
      outside: [{ run_id: "r1", created_by: "u1", state: "RUNNING", lost_reason: "reboot", proxy_release: "0.6" }],
    });
    restartAdminRunsMock.mockResolvedValue({
      results: [{ run_id: "r1", ok: false, error: serverError, lost_again: true }],
    });
    renderCard();
    await screen.findByText("Older limits · 1");
    await user.click(screen.getByRole("button", { name: "Restart with current limits" }));
    expect(await screen.findByText(new RegExp(serverError))).toBeInTheDocument();
    expect(screen.queryByText(/still lost/)).not.toBeInTheDocument();
  });
});
