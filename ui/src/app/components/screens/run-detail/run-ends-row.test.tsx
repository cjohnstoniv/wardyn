/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5): the run header's Ends and wait pill menus (#3034) —
// extending is always allowed (the lease); shortening/No end/the wait need
// the run's own captured user_changes_limits gate.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { makeRun } from "../../../../test/factories";
import type { RunDetail } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RunEndsControls } from "./run-ends-row";

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
// The pills are menus: click one, then pick an item by name.
const openMenu = async (testid: "run-ends-pill" | "run-waits-pill") => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  await user.click(screen.getByTestId(testid));
  return user;
};
const pickEnds = async (item: string | RegExp) => {
  const user = await openMenu("run-ends-pill");
  await user.click(await screen.findByRole("menuitem", { name: item }));
  return user;
};

function renderRow(run: RunDetail, onChanged: () => void = () => {}, opts: { operator?: boolean; principal?: string } = {}) {
  return render(
    <OperatorProvider operator={opts.operator ?? false} principal={opts.principal ?? OWNER}>
      <RunEndsControls run={run} onChanged={onChanged} />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  setRunEndAndWaitMock.mockResolvedValue({ id: "run-1", ends_at: null, wait_budget_sec: 0, capped: [] });
});

const endsAt = () => new Date(NOW + 8 * 3600_000).toISOString();

describe("RunEndsControls — the Ends pill (mock 'Ends')", () => {
  it("No end: the pill reads 'Ends: never'; its menu holds the explanation and, if gated, Set an end…", async () => {
    renderRow(detail({ run_limits: { user_changes_limits: true, allow_no_end: true, max_end_ahead_sec: 7 * 24 * 3600 } }));
    expect(screen.getByTestId("run-ends-pill")).toHaveTextContent("Ends: never");
    await openMenu("run-ends-pill");
    expect(await screen.findByText(/Keeps going until you or an admin ends it/)).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Set an end…/ })).toBeInTheDocument();
    expect(screen.getByText(/You can set it up to 7 days ahead/)).toBeInTheDocument();
  });

  it("No end, ungated: the menu still explains, but offers no Set an end…", async () => {
    renderRow(detail({ run_limits: { user_changes_limits: false } }));
    await openMenu("run-ends-pill");
    expect(await screen.findByText(/Keeps going until you or an admin ends it/)).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /Set an end…/ })).not.toBeInTheDocument();
  });

  it("has an end: the pill names it ('Ends: <when>'), not a sentence", () => {
    renderRow(detail({ ends_at: endsAt(), run_limits: { user_changes_limits: true } }));
    expect(screen.getByTestId("run-ends-pill").textContent).toMatch(/^Ends: \S/);
    expect(screen.getByTestId("run-ends-pill").textContent).not.toMatch(/never/);
  });

  it("has an end, locked by an admin: the pill stays a greyed menu that says why, extends stay, no Change… (R6)", async () => {
    renderRow(
      detail({ ends_at: endsAt(), run_limits: { user_changes_limits: false, max_end_ahead_sec: 30 * 24 * 3600 } }),
    );
    const pill = screen.getByTestId("run-ends-pill");
    expect(pill.tagName).toBe("BUTTON");
    expect(pill).toHaveClass("opacity-70");
    // The sentence lives in the menu, not on the facts row.
    expect(screen.queryByText(/your admin sets the rest/)).not.toBeInTheDocument();
    await openMenu("run-ends-pill");
    const why = await screen.findByTestId("lock-reason");
    expect(why).toHaveTextContent("You can extend it up to 30 days ahead; your admin sets the rest.");
    expect(why.textContent).not.toMatch(/^Ends /);
    expect(screen.getByRole("menuitem", { name: "1 more day" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Change…" })).not.toBeInTheDocument();
  });

  it("has an end, gated: the extends AND Change… are in the menu", async () => {
    renderRow(detail({ ends_at: endsAt(), run_limits: { user_changes_limits: true, max_end_ahead_sec: 30 * 24 * 3600 } }));
    await openMenu("run-ends-pill");
    expect(await screen.findByRole("menuitem", { name: "1 more day" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Change…" })).toBeInTheDocument();
  });

  // F15 (PR #1317 review): AgentRun.RunLimits has no omitempty, so the server ALWAYS sends a run_limits object; a
  // present-but-all-zero one reads user_changes_limits as the Go zero value, false: LOCKED, never "fully open".
  it("an all-zero-value run_limits object: locked, matching the server's own zero-value default", async () => {
    renderRow(detail({ ends_at: endsAt(), run_limits: {} }));
    await openMenu("run-ends-pill");
    await screen.findByRole("menuitem", { name: "1 more day" });
    expect(screen.queryByRole("menuitem", { name: "Change…" })).not.toBeInTheDocument();
  });
});

describe("RunEndsControls — terminal, lost and queued runs render nothing", () => {
  it("a QUEUED run: its end is written when it starts, so no 'Ends: never' and no Set an end…", () => {
    const { container } = renderRow(detail({ state: "QUEUED" }));
    expect(container).toBeEmptyDOMElement();
  });

  it.each(["COMPLETED", "KILLED", "FAILED", "STOPPED", "ARCHIVED"])("state=%s", (state) => {
    const { container } = renderRow(detail({ state }));
    expect(container).toBeEmptyDOMElement();
  });

  it("a lease-ended run (state RUNNING, lost_at set): the banner owns this run, not the controls", () => {
    const run = detail({ state: "RUNNING", lost_reason: "ended", lost_at: new Date(NOW - 60_000).toISOString() });
    const { container } = renderRow(run);
    expect(container).toBeEmptyDOMElement();
  });
});

// #2112: the server refuses No end on a run that has an end and a lifetime cap (run_end_wait.go planEnd), for an
// operator too, so the dialog must not offer a choice that always fails.
describe("RunEndsControls — No end on a run with a lifetime cap (#2112)", () => {
  const openChange = async (run: RunDetail) => {
    renderRow(run, () => {}, { operator: true });
    await pickEnds("Change…");
    await screen.findByRole("dialog", { name: "When this run ends" });
  };

  it("offers No end when the run has no lifetime cap", async () => {
    await openChange(detail({ ends_at: endsAt(), run_limits: { allow_no_end: true } }));
    expect(screen.getByRole("checkbox", { name: "No end" })).toBeInTheDocument();
  });

  it("hides No end when the run has a lifetime cap", async () => {
    await openChange(
      detail({ ends_at: endsAt(), lifetime_cap_at: new Date(NOW + 24 * 3600_000).toISOString(), run_limits: { allow_no_end: true } }),
    );
    expect(screen.queryByRole("checkbox", { name: "No end" })).not.toBeInTheDocument();
  });
});

describe("RunEndsControls — Extend", () => {
  it("'1 more day' extends the run's END, not now, and refreshes on success", async () => {
    const onChanged = vi.fn();
    const currentEnd = NOW + 8 * 3600_000;
    const run = detail({ ends_at: new Date(currentEnd).toISOString(), run_limits: { user_changes_limits: true } });
    renderRow(run, onChanged);
    await pickEnds("1 more day");
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    expect(setRunEndAndWaitMock).toHaveBeenCalledTimes(1);
    const [runId, body] = setRunEndAndWaitMock.mock.calls[0];
    expect(runId).toBe(run.id);
    // Within a second of currentEnd + 1 day, not now + 1 day.
    expect(Math.abs(Date.parse(body.endsAt) - (currentEnd + 24 * 3600_000))).toBeLessThan(1000);
  });

  it("a capped extend toasts the real ceiling instead of silently accepting the ask", async () => {
    const latestEnd = new Date(NOW + 30 * 24 * 3600_000).toISOString();
    setRunEndAndWaitMock.mockResolvedValue({ id: "run-1", ends_at: latestEnd, wait_budget_sec: 0, capped: ["ends_at"], latest_end: latestEnd });
    renderRow(detail({ ends_at: endsAt(), run_limits: { user_changes_limits: true, max_end_ahead_sec: 30 * 24 * 3600 } }));
    await pickEnds(/As far as allowed \(30 days\)/);
    await waitFor(() => expect(toastWarningMock).toHaveBeenCalledWith(expect.stringMatching(/as far as your admin allows/)));
  });

  it("#1322: a capped extend the admin has since loosened says so instead of the capped sentence", async () => {
    const latestEnd = new Date(NOW + 30 * 24 * 3600_000).toISOString();
    setRunEndAndWaitMock.mockResolvedValue({
      id: "run-1",
      ends_at: latestEnd,
      wait_budget_sec: 0,
      capped: ["ends_at"],
      latest_end: latestEnd,
      ends_cap_loosened: true,
    });
    renderRow(detail({ ends_at: endsAt(), run_limits: { user_changes_limits: true, max_end_ahead_sec: 30 * 24 * 3600 } }));
    await pickEnds(/As far as allowed \(30 days\)/);
    await waitFor(() =>
      expect(toastWarningMock).toHaveBeenCalledWith(
        "Your admin loosened this after the run started. Start a new run to get the new limit.",
      ),
    );
    expect(toastWarningMock).not.toHaveBeenCalledWith(expect.stringMatching(/as far as your admin allows/));
  });

  it("a 1-day limit reads '(1 day)', not '(1 days)'", async () => {
    renderRow(detail({ ends_at: endsAt(), run_limits: { user_changes_limits: true, max_end_ahead_sec: 24 * 3600 } }));
    await openMenu("run-ends-pill");
    expect(await screen.findByRole("menuitem", { name: "As far as allowed (1 day)" })).toBeInTheDocument();
  });
});

describe("RunEndsControls — Set an end… (F7: Save on the untouched default must still send a PATCH)", () => {
  it("opening Set an end… and pressing Save with the prefilled default sends a real PATCH", async () => {
    const run = detail({ run_limits: { user_changes_limits: true, allow_no_end: true } });
    renderRow(run);
    const user = await pickEnds(/Set an end…/);
    // The heading says what is being set, not the item's own label (#2921).
    expect(await screen.findByRole("dialog", { name: "When this run ends" })).toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Save" }));
    await waitFor(() => expect(setRunEndAndWaitMock).toHaveBeenCalledTimes(1));
    const [runId, body] = setRunEndAndWaitMock.mock.calls[0];
    expect(runId).toBe(run.id);
    // The prefilled default is "now + 1 day", a real future timestamp, not null/undefined.
    expect(body.endsAt).toBeTruthy();
    expect(Date.parse(body.endsAt)).toBeGreaterThan(Date.now());
  });
});

describe("RunEndsControls — the Waits pill", () => {
  it("renders only when wait_budget_sec is a real number on the wire", () => {
    renderRow(detail({ wait_budget_sec: 4 * 3600, run_limits: { user_changes_limits: true } }));
    expect(screen.getByTestId("run-waits-pill")).toHaveTextContent("Waits up to 4 h");
    cleanup();
    renderRow(detail({}));
    expect(screen.queryByTestId("run-waits-pill")).not.toBeInTheDocument();
  });

  it("locked by an admin: a greyed menu pill whose menu says why, with nothing to pick (R6)", async () => {
    renderRow(detail({ wait_budget_sec: 4 * 3600, run_limits: { user_changes_limits: false } }));
    const pill = screen.getByTestId("run-waits-pill");
    expect(pill.tagName).toBe("BUTTON");
    expect(pill).toHaveClass("opacity-70");
    expect(screen.queryByText(/Your admin sets this/)).not.toBeInTheDocument();
    await openMenu("run-waits-pill");
    expect(await screen.findByTestId("lock-reason")).toHaveTextContent("Kept for up to 4 hours. Your admin sets this.");
    expect(screen.queryByRole("menuitem")).not.toBeInTheDocument();
  });

  it("a sub-hour or fractional budget is shown in minutes, not rounded to whole hours", () => {
    renderRow(detail({ wait_budget_sec: 1200, run_limits: { user_changes_limits: true } }));
    expect(screen.getByTestId("run-waits-pill")).toHaveTextContent("Waits up to 20 min");
    cleanup();
    renderRow(detail({ wait_budget_sec: 5400, run_limits: { user_changes_limits: true } }));
    expect(screen.getByTestId("run-waits-pill")).toHaveTextContent("Waits up to 1 h 30 min");
  });

  it("gated: the menu explains the wait, marks the current budget, and a pick sends waitBudgetSec", async () => {
    const onChanged = vi.fn();
    renderRow(detail({ wait_budget_sec: 4 * 3600, run_limits: { user_changes_limits: true } }), onChanged);
    const user = await openMenu("run-waits-pill");
    expect(await screen.findByText("If this run waits for a decision")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Keep it for up to 4 hours" })).toHaveAttribute("aria-disabled", "true");
    await user.click(screen.getByRole("menuitem", { name: "Keep it for up to 24 hours" }));
    await waitFor(() => expect(setRunEndAndWaitMock).toHaveBeenCalledWith(expect.any(String), { waitBudgetSec: 24 * 3600 }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
  });
});

describe("RunEndsControls — an operator caller is never gated, regardless of the run's captured limits", () => {
  it("a locked run (user_changes_limits false) still offers Change… to an operator", async () => {
    const run = detail({ ends_at: endsAt(), run_limits: { user_changes_limits: false, max_end_ahead_sec: 30 * 24 * 3600 } });
    renderRow(run, () => {}, { operator: true, principal: "someone-else" });
    await openMenu("run-ends-pill");
    expect(await screen.findByRole("menuitem", { name: "Change…" })).toBeInTheDocument();
    expect(screen.queryByTestId("lock-reason")).not.toBeInTheDocument();
    expect(screen.getByTestId("run-ends-pill")).not.toHaveClass("opacity-70");
  });

  it("an all-zero-value run_limits OBJECT (an operator's own unassigned run) is still fully open", async () => {
    renderRow(detail({ run_limits: {} }), () => {}, { operator: true, principal: "someone-else" });
    await openMenu("run-ends-pill");
    expect(await screen.findByRole("menuitem", { name: /Set an end…/ })).toBeInTheDocument();
  });
});

// F16 (PR #1317 review): ownsRunOrSuperAdmin (run_end_wait.go) is the server's OWN gate on every write here, so a
// security admin looking at a run they neither own nor superadmin over gets a 403 on all of them; the client must
// not dangle a control that always fails.
describe("RunEndsControls — F16: a non-owner, non-operator viewer gets no action controls", () => {
  it("foreign viewer: both pills are static text, not menus, and nothing tells them what they may extend", () => {
    renderRow(
      detail({
        ends_at: endsAt(),
        wait_budget_sec: 4 * 3600,
        run_limits: { user_changes_limits: false, allow_no_end: true, max_end_ahead_sec: 7 * 24 * 3600 },
      }),
      () => {},
      { operator: false, principal: "a-security-admin" },
    );
    expect(screen.getByTestId("run-ends-pill").tagName).toBe("SPAN");
    expect(screen.getByTestId("run-waits-pill").tagName).toBe("SPAN");
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    // A viewer who cannot extend must not read "You can extend it up to 7 days ahead".
    expect(screen.queryByText(/You can extend it/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Your admin sets this/)).not.toBeInTheDocument();
  });

  it("no-end run, foreign viewer: no Set an end…, but the explanation still shows", async () => {
    renderRow(detail({ run_limits: { user_changes_limits: true, allow_no_end: true } }), () => {}, {
      operator: false,
      principal: "a-security-admin",
    });
    await openMenu("run-ends-pill");
    expect(await screen.findByText(/Keeps going until you or an admin ends it/)).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /Set an end…/ })).not.toBeInTheDocument();
  });
});
