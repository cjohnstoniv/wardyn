/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5): the run page's persistent Ends/wait row —
// extending is always allowed (the lease); shortening/No end/the wait need
// the run's own captured user_changes_limits gate.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { makeRun } from "../../../../test/factories";
import type { RunDetail } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RunEndsRow } from "./run-ends-row";

const setRunEndAndWaitMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { setRunEndAndWait: (...a: unknown[]) => setRunEndAndWaitMock(...a) },
}));
const toastWarningMock = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), warning: (...a: unknown[]) => toastWarningMock(...a) },
}));

const NOW = Date.now();
const detail = (o: Partial<RunDetail> = {}): RunDetail => makeRun(o) as RunDetail;

// Every case below is about the run's OWN captured gate, so it renders as a
// non-operator caller unless a case says otherwise — an operator bypasses the
// gate entirely (its own describe block below), which would mask what these
// cases mean to prove.
function renderRow(run: RunDetail, onChanged: () => void = () => {}, operator = false) {
  return render(
    <OperatorProvider operator={operator}>
      <RunEndsRow run={run} onChanged={onChanged} />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  setRunEndAndWaitMock.mockResolvedValue({ id: "run-1", ends_at: null, wait_budget_sec: 0, capped: [] });
});

describe("RunEndsRow — the four states (mock 'Ends')", () => {
  it("No end, gated: offers Set an end…", () => {
    const run = detail({ run_limits: { user_changes_limits: true, allow_no_end: true } });
    renderRow(run);
    expect(screen.getByText("No end")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set an end…" })).toBeInTheDocument();
  });

  it("has an end, ungated: the locked hint, no Change button", () => {
    const run = detail({
      ends_at: new Date(NOW + 8 * 3600_000).toISOString(),
      run_limits: { user_changes_limits: false, max_end_ahead_sec: 30 * 24 * 3600 },
    });
    renderRow(run);
    expect(screen.getByText(/your admin sets the rest/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change…" })).not.toBeInTheDocument();
    // Extending within the max is still always offered.
    expect(screen.getByRole("button", { name: "Extend" })).toBeInTheDocument();
  });

  it("has an end, gated: Extend AND Change… both render", () => {
    const run = detail({
      ends_at: new Date(NOW + 8 * 3600_000).toISOString(),
      run_limits: { user_changes_limits: true, max_end_ahead_sec: 30 * 24 * 3600 },
    });
    renderRow(run);
    expect(screen.getByRole("button", { name: "Extend" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Change…" })).toBeInTheDocument();
  });

  it("no run_limits at all (unassigned/super-admin owner): treated as fully open, never locked", () => {
    const run = detail({ ends_at: new Date(NOW + 8 * 3600_000).toISOString() });
    renderRow(run);
    expect(screen.getByRole("button", { name: "Change…" })).toBeInTheDocument();
    expect(screen.queryByText(/your admin sets the rest/)).not.toBeInTheDocument();
  });
});

describe("RunEndsRow — terminal and lost runs render nothing here", () => {
  it.each(["COMPLETED", "KILLED", "FAILED", "STOPPED", "ARCHIVED"])("state=%s", (state) => {
    const { container } = renderRow(detail({ state }));
    expect(container).toBeEmptyDOMElement();
  });

  it("a lease-ended run (state RUNNING, lost_at set): the banner owns this run, not the row", () => {
    const run = detail({ state: "RUNNING", lost_reason: "ended", lost_at: new Date(NOW - 60_000).toISOString() });
    const { container } = renderRow(run);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("RunEndsRow — Extend", () => {
  it("'1 more day' extends the run's END, not now — and refreshes on success", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onChanged = vi.fn();
    const currentEnd = NOW + 8 * 3600_000;
    const run = detail({ ends_at: new Date(currentEnd).toISOString(), run_limits: { user_changes_limits: true } });
    renderRow(run, onChanged);
    await user.click(screen.getByRole("button", { name: "Extend" }));
    await user.click(await screen.findByText("1 more day"));
    expect(setRunEndAndWaitMock).toHaveBeenCalledTimes(1);
    const [runId, body] = setRunEndAndWaitMock.mock.calls[0];
    expect(runId).toBe(run.id);
    // Within a second of currentEnd + 1 day, not now + 1 day.
    expect(Math.abs(Date.parse(body.endsAt) - (currentEnd + 24 * 3600_000))).toBeLessThan(1000);
  });

  it("a capped extend toasts the real ceiling instead of silently accepting the ask", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const latestEnd = new Date(NOW + 30 * 24 * 3600_000).toISOString();
    setRunEndAndWaitMock.mockResolvedValue({
      id: "run-1",
      ends_at: latestEnd,
      wait_budget_sec: 0,
      capped: ["ends_at"],
      latest_end: latestEnd,
    });
    const run = detail({
      ends_at: new Date(NOW + 8 * 3600_000).toISOString(),
      run_limits: { user_changes_limits: true, max_end_ahead_sec: 30 * 24 * 3600 },
    });
    renderRow(run);
    await user.click(screen.getByRole("button", { name: "Extend" }));
    await user.click(await screen.findByText(/As far as allowed/));
    await waitFor(() => expect(toastWarningMock).toHaveBeenCalledWith(expect.stringMatching(/as far as your admin allows/)));
  });
});

describe("RunEndsRow — the wait row", () => {
  it("renders only when wait_budget_sec is a real number on the wire", () => {
    const withWait = detail({ wait_budget_sec: 4 * 3600 });
    renderRow(withWait);
    expect(screen.getByText(/waits for a decision/)).toBeInTheDocument();

    const withoutWait = detail({});
    const { container } = renderRow(withoutWait);
    expect(container.querySelector('[data-testid="run-ends-row"]')?.textContent).not.toMatch(/waits for a decision/);
  });

  it("ungated: a static, locked sentence — no dropdown", () => {
    const run = detail({ wait_budget_sec: 4 * 3600, run_limits: { user_changes_limits: false } });
    renderRow(run);
    expect(screen.getByText("Kept for up to 4 hours. Your admin sets this.")).toBeInTheDocument();
  });
});

describe("RunEndsRow — an operator caller is never gated, regardless of the run's captured limits", () => {
  it("a locked run (user_changes_limits false) still offers Change… to an operator", () => {
    const run = detail({
      ends_at: new Date(NOW + 8 * 3600_000).toISOString(),
      run_limits: { user_changes_limits: false, max_end_ahead_sec: 30 * 24 * 3600 },
    });
    renderRow(run, () => {}, true);
    expect(screen.getByRole("button", { name: "Change…" })).toBeInTheDocument();
    expect(screen.queryByText(/your admin sets the rest/)).not.toBeInTheDocument();
  });

  it("an all-zero-value run_limits OBJECT (an operator's own unassigned run) is still fully open", () => {
    const run = detail({ run_limits: {} });
    renderRow(run, () => {}, true);
    expect(screen.getByRole("button", { name: "Set an end…" })).toBeInTheDocument();
  });
});
