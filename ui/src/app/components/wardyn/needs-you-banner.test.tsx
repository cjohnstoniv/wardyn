/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2963 "Needs you" banner (mock M8 Option A): the request, the takeover strip, the one-click hand
// back and its confirmation — the strings are the boards', through copy/needs-you.ts.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ApprovalRequest, RunDetail } from "../../lib/types";
import { NEEDS_YOU } from "./copy/needs-you";

const api = vi.hoisted(() => ({ takeOver: vi.fn(), handBack: vi.fn(), decline: vi.fn() }));
vi.mock("../../lib/api/approvals", () => ({ approvals: api }));
vi.mock("./operator-context", () => ({
  usePrincipal: () => "dev-sub",
  useOperator: () => false,
  useOperatorResolved: () => false,
}));

import { NeedsYouBanner, activeTakeover } from "./needs-you-banner";

const REASON = "I reached the staging portal's sign-in page. It sends a code to a phone, so I can't finish it.";
const run = { id: "run-1", created_by: "dev-sub", state: "RUNNING" } as unknown as RunDetail;

function request(over: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "ap-1",
    run_id: "run-1",
    kind: "human_takeover",
    requested_scope: { reason: REASON },
    state: "PENDING",
    requested_at: new Date(Date.now() - 2 * 60_000).toISOString(),
    ...over,
  };
}

function mount(approvals: ApprovalRequest[], r: RunDetail = run) {
  const cb = { onChanged: vi.fn(), onOpenTerminal: vi.fn(), onGoAudit: vi.fn() };
  render(<NeedsYouBanner run={r} approvals={approvals} {...cb} />);
  return cb;
}

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
});

describe("NeedsYouBanner", () => {
  it("quotes the agent and offers Take over, Snooze and Decline", () => {
    mount([request()]);
    const b = screen.getByTestId("needs-you-banner");
    expect(within(b).getByText(NEEDS_YOU.banner.title)).toBeInTheDocument();
    expect(within(b).getByTestId("needs-you-reason").textContent).toBe(REASON);
    expect(
      within(b).getByText("Asked 2 min ago. Taking over opens the terminal. The agent waits until you take over or decline."),
    ).toBeInTheDocument();
    for (const name of [NEEDS_YOU.banner.takeOver, NEEDS_YOU.banner.snooze, NEEDS_YOU.banner.decline]) {
      expect(within(b).getByRole("button", { name: new RegExp(name) })).toBeInTheDocument();
    }
  });

  it("Take over on a run with no desktop opens the terminal", async () => {
    api.takeOver.mockResolvedValue({ ...request({ state: "APPROVED" }), surface: "terminal" });
    const cb = mount([request()]);
    await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.takeOver }));
    expect(api.takeOver).toHaveBeenCalledWith("ap-1");
    expect(cb.onOpenTerminal).toHaveBeenCalled();
    expect(cb.onChanged).toHaveBeenCalled();
  });

  it("while you have control: the strip claims no recording pause, and Hand back is one click to the confirmation", async () => {
    const decided = new Date(Date.now() - 302_000).toISOString();
    const handed = new Date(Date.parse(decided) + 302_000).toISOString();
    api.handBack.mockResolvedValue(request({ state: "APPROVED", decided_at: decided, handed_back_at: handed, takeover_holder: "dev-sub" }));
    const cb = mount([request({ state: "APPROVED", decided_at: decided, takeover_holder: "dev-sub" })]);
    const strip = screen.getByTestId("needs-you-control");
    expect(within(strip).getByText(NEEDS_YOU.strip.control("terminal"))).toBeInTheDocument();
    // A recorded run: no recorder pauses for a takeover yet, so the strip says nothing about recording.
    expect(within(strip).queryByTestId("needs-you-recording")).toBeNull();
    expect(strip.textContent).not.toMatch(/paus/i);
    await userEvent.click(within(strip).getByRole("button", { name: NEEDS_YOU.strip.handBack }));
    expect(api.handBack).toHaveBeenCalledWith("ap-1");
    const done = screen.getByTestId("needs-you-done");
    expect(within(done).getByText(NEEDS_YOU.done.title)).toBeInTheDocument();
    expect(within(done).getByText(NEEDS_YOU.done.working)).toBeInTheDocument();
    expect(done.textContent).not.toMatch(/recording|gap/i);
    await userEvent.click(within(done).getByRole("button", { name: NEEDS_YOU.done.audit }));
    expect(cb.onGoAudit).toHaveBeenCalled();
  });

  it("a run that is not recorded says Recording off, and nothing about pausing", () => {
    mount([request({ state: "APPROVED", decided_at: new Date().toISOString(), takeover_holder: "dev-sub" })], {
      ...run,
      not_recorded: "org_off",
    } as RunDetail);
    const off = screen.getByTestId("needs-you-recording");
    expect(off.textContent).toBe(NEEDS_YOU.strip.off);
    expect(off.getAttribute("title")).toBe("This run isn't being recorded.");
  });

  it("someone else holding control is shown in use, with Take over", () => {
    mount([request({ state: "APPROVED", decided_at: new Date().toISOString(), takeover_holder: "priya" })]);
    const tile = screen.getByTestId("needs-you-in-use");
    expect(within(tile).getByText(NEEDS_YOU.inUse.detail("priya", "terminal"))).toBeInTheDocument();
    expect(within(tile).getByRole("button", { name: NEEDS_YOU.banner.takeOver })).toBeInTheDocument();
  });

  it("Decline answers the request", async () => {
    api.decline.mockResolvedValue(request({ state: "DENIED" }));
    const cb = mount([request()]);
    await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.decline }));
    expect(api.decline).toHaveBeenCalledWith("ap-1");
    expect(cb.onChanged).toHaveBeenCalled();
  });

  it("Snooze shrinks the banner to one line that still offers Take over", async () => {
    mount([request()]);
    await userEvent.click(screen.getByRole("button", { name: new RegExp(NEEDS_YOU.banner.snooze) }));
    await userEvent.click(await screen.findByRole("menuitem", { name: NEEDS_YOU.banner.snooze10 }));
    const line = screen.getByTestId("needs-you-snoozed");
    expect(line.textContent).toMatch(/^Snoozed until /);
    expect(within(line).getByRole("button", { name: NEEDS_YOU.banner.takeOver })).toBeInTheDocument();
    expect(screen.queryByTestId("needs-you-banner")).toBeNull();
  });

  it("a handed-back, declined or other-kind row is not active", () => {
    expect(activeTakeover([request({ state: "APPROVED", handed_back_at: new Date().toISOString() }), request({ state: "DENIED" }), request({ kind: "tool_call" })])).toBeUndefined();
  });
});
