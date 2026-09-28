/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5): the run page's one lifetime banner — lost/ended/
// paused/ending-soon are mutually exclusive server facts, so this pins each
// state's own copy and actions, and that only one ever renders.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { makeRun } from "../../../../test/factories";
import type { ApprovalRequest, RunDetail } from "../../../lib/types";
import { RunLifetimeBanner } from "./run-lifetime-banner";

const reviveRunMock = vi.fn();
const setRunEndAndWaitMock = vi.fn();
const killRunMock = vi.fn();
const resumeRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    reviveRun: (...a: unknown[]) => reviveRunMock(...a),
    setRunEndAndWait: (...a: unknown[]) => setRunEndAndWaitMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
    resumeRun: (...a: unknown[]) => resumeRunMock(...a),
  },
}));
const toastInfoMock = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), warning: vi.fn(), info: (...a: unknown[]) => toastInfoMock(...a) },
}));

const NOW = Date.now();
const detail = (o: Partial<RunDetail> = {}): RunDetail => makeRun(o) as RunDetail;

beforeEach(() => {
  vi.clearAllMocks();
  reviveRunMock.mockResolvedValue({ run_id: "run-1", denied_added: [], proxy_release: "r1" });
  setRunEndAndWaitMock.mockResolvedValue({ id: "run-1", ends_at: null, wait_budget_sec: 0, capped: [] });
  killRunMock.mockResolvedValue(undefined);
  resumeRunMock.mockResolvedValue(undefined);
});

describe("RunLifetimeBanner — lost (reboot/outage)", () => {
  it("a Claude Code run lost to a reboot: LOST_TITLE, the Claude-continues body, Revive/Extend/End run", () => {
    const run = detail({ lost_reason: "reboot", ends_at: new Date(NOW + 3600_000).toISOString(), agent: "claude-code" });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(screen.getByText("This run's sandbox stopped")).toBeInTheDocument();
    expect(screen.getByText(/continues the Claude Code conversation/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Revive" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Extend" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "End run" })).toBeInTheDocument();
    // Reboot, not outage: the outage-only sentence must not appear.
    expect(screen.queryByText(/unreachable for over an hour/)).not.toBeInTheDocument();
  });

  it("a non-Claude harness lost to an outage: the other-agent body PLUS the outage sentence", () => {
    const run = detail({ lost_reason: "outage", agent: "codex-cli" });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(screen.getByText(/starts a new session/)).toBeInTheDocument();
    expect(screen.getByText(/unreachable for over an hour/)).toBeInTheDocument();
  });

  it("clicking Revive calls reviveRun and reports newly-blocked hosts, then refreshes", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    reviveRunMock.mockResolvedValue({ run_id: "run-1", denied_added: ["a.com", "b.com"], proxy_release: "r1" });
    const onChanged = vi.fn();
    const run = detail({ lost_reason: "reboot" });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={onChanged} />);
    await user.click(screen.getByRole("button", { name: "Revive" }));
    await waitFor(() => expect(reviveRunMock).toHaveBeenCalledWith("run-1"));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(toastInfoMock).toHaveBeenCalledWith("Policy updated at revive: 2 hosts now blocked.");
  });
});

describe("RunLifetimeBanner — a lease-ended run", () => {
  it("ENDED_TITLE with Extend-and-revive / End run — never the lost copy", () => {
    const run = detail({ lost_reason: "ended", lost_at: new Date(NOW - 60_000).toISOString() });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(screen.getByText("This run ended at its end time")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Extend and revive" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "End run" })).toBeInTheDocument();
    expect(screen.queryByText("This run's sandbox stopped")).not.toBeInTheDocument();
  });

  it("Extend and revive sets a fresh future end BEFORE reviving, in order", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const order: string[] = [];
    setRunEndAndWaitMock.mockImplementation(async () => {
      order.push("end");
      return { id: "run-1", ends_at: null, wait_budget_sec: 0, capped: [] };
    });
    reviveRunMock.mockImplementation(async () => {
      order.push("revive");
      return { run_id: "run-1", denied_added: [], proxy_release: "r1" };
    });
    const run = detail({ lost_reason: "ended" });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "Extend and revive" }));
    await waitFor(() => expect(order).toEqual(["end", "revive"]));
    const [, patchBody] = setRunEndAndWaitMock.mock.calls[0];
    expect(Date.parse(patchBody.endsAt)).toBeGreaterThan(Date.now());
  });
});

describe("RunLifetimeBanner — paused", () => {
  it("waiting on a decision: reads the open request's own expires_at, offers Resume now", () => {
    const until = new Date(NOW + 3600_000).toISOString();
    const run = detail({ paused_at: new Date(NOW - 60_000).toISOString(), paused_reason: "waiting" });
    const pending: ApprovalRequest[] = [
      { id: "a1", run_id: "run-1", kind: "egress_domain", requested_scope: {}, state: "PENDING", requested_at: NOW.toString(), expires_at: until },
    ];
    render(<RunLifetimeBanner run={run} pending={pending} onChanged={vi.fn()} />);
    expect(screen.getByText("Paused while waiting for approval")).toBeInTheDocument();
    expect(screen.getByText(/The request stays open until/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resume now" })).toBeInTheDocument();
  });

  it("idle: the minutes come from the run's OWN captured pause_idle_after_sec", () => {
    const run = detail({
      paused_at: new Date(NOW - 60_000).toISOString(),
      paused_reason: "idle",
      run_limits: { pause_idle_after_sec: 1800 },
    });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(screen.getByText("Paused — nobody's been here for 30 minutes")).toBeInTheDocument();
  });

  it("clicking Resume now calls resumeRun", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onChanged = vi.fn();
    const run = detail({ paused_at: new Date(NOW - 60_000).toISOString(), paused_reason: "idle" });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={onChanged} />);
    await user.click(screen.getByRole("button", { name: "Resume now" }));
    await waitFor(() => expect(resumeRunMock).toHaveBeenCalledWith("run-1"));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });
});

describe("RunLifetimeBanner — ending soon and the quiet default", () => {
  it("10 minutes left: the warning banner, dismissible", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const run = detail({ ends_at: new Date(NOW + 9 * 60_000).toISOString() });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(screen.getByText("This run ends in 10 minutes")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("This run ends in 10 minutes")).not.toBeInTheDocument();
  });

  it("a tightened profile note renders when nothing more urgent is going on", () => {
    const run = detail({
      ends_at: new Date(NOW + 5 * 24 * 3600_000).toISOString(),
      end_tightened_at: new Date(NOW - 60_000).toISOString(),
    });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(screen.getByText(/Your admin shortened the limit/)).toBeInTheDocument();
  });

  it("nothing lost, ended, paused, ending-soon or tightened: renders nothing", () => {
    const run = detail({ ends_at: new Date(NOW + 30 * 24 * 3600_000).toISOString() });
    const { container } = render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("lost always wins over a simultaneous ending-soon window", () => {
    const run = detail({ lost_reason: "reboot", ends_at: new Date(NOW + 9 * 60_000).toISOString() });
    render(<RunLifetimeBanner run={run} pending={[]} onChanged={vi.fn()} />);
    expect(screen.getByText("This run's sandbox stopped")).toBeInTheDocument();
    expect(screen.queryByText(/This run ends in/)).not.toBeInTheDocument();
  });
});
