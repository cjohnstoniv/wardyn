/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5): the run page's one lifetime banner — lost/ended/
// paused/ending-soon are mutually exclusive server facts, so this pins each
// state's own copy and actions, and that only one ever renders (the
// tightened note is the one exception, F19).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { makeRun } from "../../../../test/factories";
import type { ApprovalRequest, RunDetail } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RunLifetimeBanner } from "./run-lifetime-banner";
import { weekdayClock } from "./run-ends-row";

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
const OWNER = "test-user";
const detail = (o: Partial<RunDetail> = {}): RunDetail => makeRun({ created_by: OWNER, ...o }) as RunDetail;

// The default context (DEFAULT_ME_IDENTITY) is fail-open operator:true, which
// is why plain `render` below still sees every action button — this helper
// is only needed where a case cares about a SPECIFIC identity (F16).
function renderBanner(
  run: RunDetail,
  pending: ApprovalRequest[] = [],
  onChanged: () => void = () => {},
  opts: { operator?: boolean; principal?: string } = {},
) {
  return render(
    <OperatorProvider operator={opts.operator ?? true} principal={opts.principal ?? OWNER}>
      <RunLifetimeBanner run={run} pending={pending} onChanged={onChanged} />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  reviveRunMock.mockResolvedValue({ run_id: "run-1", denied_added: [], proxy_release: "r1" });
  setRunEndAndWaitMock.mockResolvedValue({ id: "run-1", ends_at: null, wait_budget_sec: 0, capped: [] });
  killRunMock.mockResolvedValue(undefined);
  resumeRunMock.mockResolvedValue(undefined);
});

describe("RunLifetimeBanner — terminal guard (F3)", () => {
  it.each(["KILLED", "STOPPED", "FAILED", "COMPLETED", "ARCHIVED"])(
    "a %s run that was lost to a reboot renders NOTHING — only a revive clears lost_*, never a terminal transition",
    (state) => {
      const run = detail({ state, lost_reason: "reboot" });
      const { container } = renderBanner(run);
      expect(container).toBeEmptyDOMElement();
    },
  );

  it("a KILLED, lease-ended run also renders nothing (not stuck showing Extend to revive forever)", () => {
    const run = detail({ state: "KILLED", lost_reason: "ended", lost_at: new Date(NOW - 60_000).toISOString() });
    const { container } = renderBanner(run);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("RunLifetimeBanner — lost (reboot)", () => {
  it("a Claude Code run lost to a reboot: LOST_TITLE, the Claude-continues body, Revive/Extend/End run", () => {
    const run = detail({ lost_reason: "reboot", ends_at: new Date(NOW + 3600_000).toISOString(), agent: "claude-code" });
    renderBanner(run);
    expect(screen.getByText("This run's sandbox stopped")).toBeInTheDocument();
    expect(screen.getByText(/continues the Claude Code conversation/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Revive" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Extend" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "End run" })).toBeInTheDocument();
    // Reboot, not outage: the outage-only sentence must not appear.
    expect(screen.queryByText(/unreachable for over an hour/)).not.toBeInTheDocument();
  });

  it("clicking Revive calls reviveRun and reports newly-blocked hosts, then refreshes", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    reviveRunMock.mockResolvedValue({ run_id: "run-1", denied_added: ["a.com", "b.com"], proxy_release: "r1" });
    const onChanged = vi.fn();
    const run = detail({ lost_reason: "reboot" });
    renderBanner(run, [], onChanged);
    await user.click(screen.getByRole("button", { name: "Revive" }));
    await waitFor(() => expect(reviveRunMock).toHaveBeenCalledWith("run-1"));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(toastInfoMock).toHaveBeenCalledWith("Policy updated at revive: 2 hosts now blocked.");
  });

  // F8 (PR #1317 review): one shared `busy` boolean used to relabel EVERY
  // button "Reviving…" while any action was in flight.
  it("F8: clicking Extend never relabels the Revive button as Reviving…", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    let resolveExtend!: () => void;
    setRunEndAndWaitMock.mockReturnValue(new Promise((r) => { resolveExtend = () => r({ id: "run-1", ends_at: null, wait_budget_sec: 0, capped: [] }); }));
    const run = detail({ lost_reason: "reboot" });
    renderBanner(run);
    await user.click(screen.getByRole("button", { name: "Extend" }));
    // Still mid-flight (the promise above hasn't resolved) — Revive must
    // still read "Revive", and reviveRun must never have been called.
    expect(screen.getByRole("button", { name: "Revive" })).toBeInTheDocument();
    expect(reviveRunMock).not.toHaveBeenCalled();
    resolveExtend();
    await waitFor(() => expect(setRunEndAndWaitMock).toHaveBeenCalled());
  });
});

// F18 (PR #1317 round-2 review): no k8s revive in 0.8 (L6) — Revive/Extend
// stay Docker-only, but End run must still work regardless of substrate.
describe("RunLifetimeBanner — lost on k8s (F18)", () => {
  it("shows the k8s line and End run, but never Revive or Extend", () => {
    const run = detail({ lost_reason: "reboot", runner_target: "k8s" });
    renderBanner(run);
    expect(screen.getByText("This run's sandbox stopped")).toBeInTheDocument();
    expect(screen.getByText("The node it ran on restarted. Files on your drive are kept; the rest is gone.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revive" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Extend" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "End run" })).toBeInTheDocument();
  });
});

describe("RunLifetimeBanner — lost (outage): F5, one sentence, no contradiction", () => {
  it("shows ONLY LOST_OUTAGE — never the reboot body or the harness-continuity line", () => {
    const run = detail({ lost_reason: "outage", agent: "claude-code", ends_at: new Date(NOW + 3600_000).toISOString() });
    renderBanner(run);
    expect(screen.getByText(/unreachable for over an hour/)).toBeInTheDocument();
    expect(screen.queryByText(/machine it ran on restarted/)).not.toBeInTheDocument();
    expect(screen.queryByText(/continues the Claude Code conversation/)).not.toBeInTheDocument();
    expect(screen.queryByText(/starts a new session/)).not.toBeInTheDocument();
  });

  it("still offers Revive/Extend/End run", () => {
    const run = detail({ lost_reason: "outage" });
    renderBanner(run);
    expect(screen.getByRole("button", { name: "Revive" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Extend" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "End run" })).toBeInTheDocument();
  });
});

describe("RunLifetimeBanner — a lease-ended run", () => {
  it("ENDED_TITLE, ONLY the approved no-date sentence when the server sent no kept_until (R2-1), never the lost copy", () => {
    const run = detail({ lost_reason: "ended", lost_at: new Date(NOW - 60_000).toISOString(), interactive: true });
    renderBanner(run);
    expect(screen.getByText("This run ended at its end time")).toBeInTheDocument();
    // R2-1: "before its files are cleaned up" was unapproved copy nobody
    // drew — only the two sentences the owner actually approved render.
    expect(screen.getByText("It has no network. Extend to revive it.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Extend and revive" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "End run" })).toBeInTheDocument();
    expect(screen.queryByText("This run's sandbox stopped")).not.toBeInTheDocument();
  });

  // The ended run is read just after it ended, so kept_until is the grace less a
  // minute away. weekday + clock alone would repeat the moment it ended.
  const DAY = 24 * 3600_000;
  const clock = (iso: string) => new Date(iso).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  const keptRun = (graceDays: number) => {
    const keptUntil = new Date(NOW + graceDays * DAY - 60_000).toISOString();
    return { keptUntil, run: detail({ lost_reason: "ended", lost_at: new Date(NOW - 60_000).toISOString(), interactive: true, kept_until: keptUntil }) };
  };
  const body = (dateText: string) => `It has no network. Its files are kept until ${dateText}. Extend to revive it.`;

  it("#1320: a 1-day grace says the weekday and clock", () => {
    const { keptUntil, run } = keptRun(1);
    renderBanner(run);
    expect(screen.getByText(body(weekdayClock(keptUntil)))).toBeInTheDocument();
    expect(screen.queryByText(/next week/)).not.toBeInTheDocument();
    expect(screen.queryByText("It has no network. Extend to revive it.")).not.toBeInTheDocument();
  });

  it("#1320: the default 7-day grace says 'next week', not a bare weekday that repeats the end", () => {
    const { keptUntil, run } = keptRun(7);
    renderBanner(run);
    expect(screen.getByText(body(`${weekdayClock(keptUntil)} next week`))).toBeInTheDocument();
  });

  it("#1320: a 14-day grace says the full date with the clock", () => {
    const { keptUntil, run } = keptRun(14);
    renderBanner(run);
    const d = new Date(keptUntil);
    const date = d.toLocaleDateString(undefined, { day: "numeric", month: "short" });
    expect(screen.getByText(body(`${date} ${clock(keptUntil)}`))).toBeInTheDocument();
    expect(screen.queryByText(/next week/)).not.toBeInTheDocument();
  });

  it("#1320: a task run keeps ENDED_BODY_TASK even when kept_until is present", () => {
    const keptUntil = new Date(NOW + 5 * 24 * 3600_000).toISOString();
    renderBanner(detail({ lost_reason: "ended", interactive: false, kept_until: keptUntil }));
    expect(screen.getByText("It has no network.")).toBeInTheDocument();
    expect(screen.queryByText(/files are kept until/)).not.toBeInTheDocument();
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
    const run = detail({ lost_reason: "ended", interactive: true });
    renderBanner(run);
    await user.click(screen.getByRole("button", { name: "Extend and revive" }));
    await waitFor(() => expect(order).toEqual(["end", "revive"]));
    const [, patchBody] = setRunEndAndWaitMock.mock.calls[0];
    expect(Date.parse(patchBody.endsAt)).toBeGreaterThan(Date.now());
  });

  // R2-2 (PR #1317 round-2 review, F9 REJECTED): reviveEligible always
  // refuses a task run's revive, AND run_end_wait.go is explicit that
  // extending an already-ended run changes nothing observable — no network,
  // no revive, no extra file time (the grace counts from lost_at, never from
  // a later PATCH). Round 1's "Extend alone" fix was a button that does
  // nothing; the only real action left is End run.
  it("R2-2: a non-interactive (task) run offers End run ONLY — no Extend, no revive sentence", () => {
    const run = detail({ lost_reason: "ended", interactive: false });
    renderBanner(run);
    expect(screen.getByText("This run ended at its end time")).toBeInTheDocument();
    expect(screen.getByText("It has no network.")).toBeInTheDocument();
    expect(screen.queryByText(/Extend to revive it/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Extend and revive" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Extend" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "End run" })).toBeInTheDocument();
  });
});

describe("RunLifetimeBanner — paused", () => {
  it("waiting on a decision: reads the open request's own expires_at, offers Resume now", () => {
    const until = new Date(NOW + 3600_000).toISOString();
    const run = detail({ paused_at: new Date(NOW - 60_000).toISOString(), paused_reason: "waiting" });
    const pending: ApprovalRequest[] = [
      { id: "a1", run_id: "run-1", kind: "egress_domain", requested_scope: {}, state: "PENDING", requested_at: NOW.toString(), expires_at: until },
    ];
    renderBanner(run, pending);
    expect(screen.getByText("Paused while waiting for approval")).toBeInTheDocument();
    expect(screen.getByText(/The request stays open until/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resume now" })).toBeInTheDocument();
  });

  it("idle: the minutes come from the run's OWN captured pause_idle_after_sec, above the floor", () => {
    const run = detail({
      paused_at: new Date(NOW - 60_000).toISOString(),
      paused_reason: "idle",
      run_limits: { pause_idle_after_sec: 1800 },
    });
    renderBanner(run);
    expect(screen.getByText("Paused — nobody's been here for 30 minutes")).toBeInTheDocument();
  });

  // F14 (PR #1317 review): the server never pauses an idle run sooner than
  // pauseDelayFloor (630s / 10.5 min), even under a profile that sets less.
  it("F14: a setting BELOW the server's 630s floor reads the floor, not the raw setting", () => {
    const run = detail({
      paused_at: new Date(NOW - 60_000).toISOString(),
      paused_reason: "idle",
      run_limits: { pause_idle_after_sec: 300 },
    });
    renderBanner(run);
    expect(screen.getByText("Paused — nobody's been here for 11 minutes")).toBeInTheDocument();
    expect(screen.queryByText(/5 minutes/)).not.toBeInTheDocument();
  });

  it("clicking Resume now calls resumeRun", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onChanged = vi.fn();
    const run = detail({ paused_at: new Date(NOW - 60_000).toISOString(), paused_reason: "idle" });
    renderBanner(run, [], onChanged);
    await user.click(screen.getByRole("button", { name: "Resume now" }));
    await waitFor(() => expect(resumeRunMock).toHaveBeenCalledWith("run-1"));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });
});

describe("RunLifetimeBanner — ending soon (M1: Change end… joins Extend 1 day/Dismiss)", () => {
  it("10 minutes left: the warning banner, with Change end… and Dismiss", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const run = detail({ ends_at: new Date(NOW + 9 * 60_000).toISOString() });
    renderBanner(run);
    expect(screen.getByText("This run ends in 10 minutes")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Change end…" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("This run ends in 10 minutes")).not.toBeInTheDocument();
  });

  it("M1: Change end… opens the SAME Change… dialog the Ends row uses, and Save PATCHes", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const run = detail({ ends_at: new Date(NOW + 9 * 60_000).toISOString(), run_limits: { user_changes_limits: true } });
    renderBanner(run);
    await user.click(screen.getByRole("button", { name: "Change end…" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(setRunEndAndWaitMock).toHaveBeenCalledTimes(1));
  });

  // R2-3 (PR #1317 round-2 review): Change end… used to show for ANY
  // canAct, skipping the captured user_changes_limits gate run-ends-row.tsx's
  // own Change… honours — the server refuses an earlier end or No end
  // without it (run_end_wait.go), so a locked member got a dialog whose only
  // live options always 403.
  it("R2-3: a locked member (no user_changes_limits) never sees Change end…", () => {
    const run = detail({
      ends_at: new Date(NOW + 9 * 60_000).toISOString(),
      run_limits: { user_changes_limits: false },
    });
    renderBanner(run, [], () => {}, { operator: false, principal: OWNER });
    expect(screen.getByText("This run ends in 10 minutes")).toBeInTheDocument();
    // Extend is still offered — extending within the max is always allowed.
    expect(screen.getByRole("button", { name: "Extend 1 day" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change end…" })).not.toBeInTheDocument();
  });

  it("R2-3: a member the gate DOES admit sees Change end…", () => {
    const run = detail({
      ends_at: new Date(NOW + 9 * 60_000).toISOString(),
      run_limits: { user_changes_limits: true },
    });
    renderBanner(run, [], () => {}, { operator: false, principal: OWNER });
    expect(screen.getByRole("button", { name: "Change end…" })).toBeInTheDocument();
  });
});

describe("RunLifetimeBanner — F19: the tightened note sits ALONGSIDE the primary banner", () => {
  it("a tightened note renders together with the paused banner, not in place of it", () => {
    const run = detail({
      paused_at: new Date(NOW - 60_000).toISOString(),
      paused_reason: "idle",
      ends_at: new Date(NOW + 5 * 24 * 3600_000).toISOString(),
      end_tightened_at: new Date(NOW - 60_000).toISOString(),
    });
    renderBanner(run);
    expect(screen.getByText(/nobody's been here/)).toBeInTheDocument();
    expect(screen.getByText(/Your admin shortened the limit/)).toBeInTheDocument();
  });

  it("alone, when nothing more urgent is going on", () => {
    const run = detail({
      ends_at: new Date(NOW + 5 * 24 * 3600_000).toISOString(),
      end_tightened_at: new Date(NOW - 60_000).toISOString(),
    });
    renderBanner(run);
    expect(screen.getByText(/Your admin shortened the limit/)).toBeInTheDocument();
  });

  it("nothing lost, ended, paused, ending-soon or tightened: renders nothing", () => {
    const run = detail({ ends_at: new Date(NOW + 30 * 24 * 3600_000).toISOString() });
    const { container } = renderBanner(run);
    expect(container).toBeEmptyDOMElement();
  });

  it("lost always wins over a simultaneous ending-soon window", () => {
    const run = detail({ lost_reason: "reboot", ends_at: new Date(NOW + 9 * 60_000).toISOString() });
    renderBanner(run);
    expect(screen.getByText("This run's sandbox stopped")).toBeInTheDocument();
    expect(screen.queryByText(/This run ends in/)).not.toBeInTheDocument();
  });
});

// F16 (PR #1317 review): every action here is owner-or-super-admin on the
// server — a security admin (operator:false) who neither owns nor
// superadmins this run gets a 403 on all of them.
describe("RunLifetimeBanner — F16: a non-owner, non-operator viewer gets no action buttons", () => {
  it("lost: the situation still reads, but no Revive/Extend/End run", () => {
    const run = detail({ lost_reason: "reboot" });
    renderBanner(run, [], () => {}, { operator: false, principal: "a-security-admin" });
    expect(screen.getByText("This run's sandbox stopped")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revive" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Extend" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "End run" })).not.toBeInTheDocument();
  });

  it("paused: no Resume now", () => {
    const run = detail({ paused_at: new Date(NOW - 60_000).toISOString(), paused_reason: "idle" });
    renderBanner(run, [], () => {}, { operator: false, principal: "a-security-admin" });
    expect(screen.queryByRole("button", { name: "Resume now" })).not.toBeInTheDocument();
  });

  it("ending soon: Dismiss still works (it's local-only), but no Extend 1 day or Change end…", () => {
    const run = detail({ ends_at: new Date(NOW + 9 * 60_000).toISOString() });
    renderBanner(run, [], () => {}, { operator: false, principal: "a-security-admin" });
    expect(screen.getByRole("button", { name: "Dismiss" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Extend 1 day" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change end…" })).not.toBeInTheDocument();
  });
});
