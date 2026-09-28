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
// makeRun's own default created_by — renderRow's default principal matches
// it, so every case below is "the owner looking at their own run" unless it
// says otherwise (the F16 describe block flips it on purpose).
const OWNER = "test-user";
const detail = (o: Partial<RunDetail> = {}): RunDetail => makeRun({ created_by: OWNER, ...o }) as RunDetail;

// Every case below is about the run's OWN captured gate, so it renders as a
// non-operator OWNER caller unless a case says otherwise — an operator
// bypasses the gate entirely, and a non-owner non-operator sees no actions at
// all (F16); either would mask what a plain gate case means to prove.
function renderRow(run: RunDetail, onChanged: () => void = () => {}, opts: { operator?: boolean; principal?: string } = {}) {
  return render(
    <OperatorProvider operator={opts.operator ?? false} principal={opts.principal ?? OWNER}>
      <RunEndsRow run={run} onChanged={onChanged} />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  setRunEndAndWaitMock.mockResolvedValue({ id: "run-1", ends_at: null, wait_budget_sec: 0, capped: [] });
});

describe("RunEndsRow — the four states (mock 'Ends')", () => {
  it("No end, gated: offers Set an end…, and the No-end hint (M2)", () => {
    const run = detail({ run_limits: { user_changes_limits: true, allow_no_end: true } });
    renderRow(run);
    expect(screen.getByText("No end")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set an end…" })).toBeInTheDocument();
    expect(screen.getByText(/Keeps going until you or an admin ends it/)).toBeInTheDocument();
  });

  it("No end, ungated: still shows the hint, but no Set-an-end button", () => {
    const run = detail({ run_limits: { user_changes_limits: false } });
    renderRow(run);
    expect(screen.getByText(/Keeps going until you or an admin ends it/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Set an end…" })).not.toBeInTheDocument();
  });

  it("has an end, ungated: the locked hint with NO repeated date (F11), no Change button", () => {
    const run = detail({
      ends_at: new Date(NOW + 8 * 3600_000).toISOString(),
      run_limits: { user_changes_limits: false, max_end_ahead_sec: 30 * 24 * 3600 },
    });
    renderRow(run);
    const hint = screen.getByText(/your admin sets the rest/);
    expect(hint).toBeInTheDocument();
    expect(hint.textContent).not.toMatch(/^Ends /);
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

  // F15 (PR #1317 review): AgentRun.RunLimits has no omitempty, so the
  // server ALWAYS sends a run_limits object — an absent one is unreachable,
  // and a present-but-all-zero one (the closest real analogue) reads
  // user_changes_limits as the Go zero value, false — LOCKED, matching the
  // server's own planRunEndWait, never "fully open".
  it("an all-zero-value run_limits object: locked, matching the server's own zero-value default", () => {
    const run = detail({ ends_at: new Date(NOW + 8 * 3600_000).toISOString(), run_limits: {} });
    renderRow(run);
    expect(screen.queryByRole("button", { name: "Change…" })).not.toBeInTheDocument();
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
    await user.click(await screen.findByText(/As far as allowed \(30 days\)/));
    await waitFor(() => expect(toastWarningMock).toHaveBeenCalledWith(expect.stringMatching(/as far as your admin allows/)));
  });

  it("a 1-day limit reads '(1 day)', not '(1 days)'", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const run = detail({
      ends_at: new Date(NOW + 8 * 3600_000).toISOString(),
      run_limits: { user_changes_limits: true, max_end_ahead_sec: 24 * 3600 },
    });
    renderRow(run);
    await user.click(screen.getByRole("button", { name: "Extend" }));
    expect(await screen.findByText("As far as allowed (1 day)")).toBeInTheDocument();
  });
});

describe("RunEndsRow — Change… (F7: Save on the untouched default must still send a PATCH)", () => {
  it("opening Set an end… and pressing Save with the prefilled default sends a real PATCH", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onChanged = vi.fn();
    const run = detail({ run_limits: { user_changes_limits: true, allow_no_end: true } });
    renderRow(run, onChanged);
    await user.click(screen.getByRole("button", { name: "Set an end…" }));
    await user.click(await screen.findByRole("button", { name: "Save" }));
    await waitFor(() => expect(setRunEndAndWaitMock).toHaveBeenCalledTimes(1));
    const [runId, body] = setRunEndAndWaitMock.mock.calls[0];
    expect(runId).toBe(run.id);
    // The prefilled default is "now + 1 day", a real future timestamp — not
    // null/undefined, which is what the F7 bug silently sent instead.
    expect(body.endsAt).toBeTruthy();
    expect(Date.parse(body.endsAt)).toBeGreaterThan(Date.now());
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
    renderRow(run, () => {}, { operator: true, principal: "someone-else" });
    expect(screen.getByRole("button", { name: "Change…" })).toBeInTheDocument();
    expect(screen.queryByText(/your admin sets the rest/)).not.toBeInTheDocument();
  });

  it("an all-zero-value run_limits OBJECT (an operator's own unassigned run) is still fully open", () => {
    const run = detail({ run_limits: {} });
    renderRow(run, () => {}, { operator: true, principal: "someone-else" });
    expect(screen.getByRole("button", { name: "Set an end…" })).toBeInTheDocument();
  });
});

// F16 (PR #1317 review): ownsRunOrSuperAdmin (run_end_wait.go) is the
// server's OWN gate on every write here — a security admin (operator=false)
// looking at a run they neither own nor superadmin over gets a 403 on all of
// them, so the client must not dangle a control that always fails.
describe("RunEndsRow — F16: a non-owner, non-operator viewer gets no action controls", () => {
  it("gated run, foreign viewer: no Extend, no Change…, no Set an end…", () => {
    const run = detail({
      ends_at: new Date(NOW + 8 * 3600_000).toISOString(),
      run_limits: { user_changes_limits: true, allow_no_end: true },
    });
    renderRow(run, () => {}, { operator: false, principal: "a-security-admin" });
    expect(screen.queryByRole("button", { name: "Extend" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change…" })).not.toBeInTheDocument();
  });

  it("no-end run, foreign viewer: no Set an end…, but the informational hint still shows", () => {
    const run = detail({ run_limits: { user_changes_limits: true, allow_no_end: true } });
    renderRow(run, () => {}, { operator: false, principal: "a-security-admin" });
    expect(screen.queryByRole("button", { name: "Set an end…" })).not.toBeInTheDocument();
    expect(screen.getByText(/Keeps going until you or an admin ends it/)).toBeInTheDocument();
  });
});
