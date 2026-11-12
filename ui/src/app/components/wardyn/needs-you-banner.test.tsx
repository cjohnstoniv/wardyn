/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2963 "Needs you" banner (mock M8 Option A): the request, the takeover strip, the one-click hand
// back and its confirmation — the strings are the boards', through copy/needs-you.ts.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ApprovalRequest, RunDetail } from "../../lib/types";
import { NEEDS_YOU } from "./copy/needs-you";
import { RUN_SSH } from "./copy";

const api = vi.hoisted(() => ({ takeOver: vi.fn(), handBack: vi.fn(), decline: vi.fn() }));
const toastError = vi.hoisted(() => vi.fn());
vi.mock("sonner", () => ({ toast: { error: toastError, success: vi.fn() } }));
vi.mock("../../lib/api/approvals", () => ({ approvals: api }));
vi.mock("../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({}) } }));
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

// A stubbed window.open: the new tab the click opens before any request.
let tabStub: { opener: unknown; name: string; close: ReturnType<typeof vi.fn>; location: { replace: ReturnType<typeof vi.fn> } };
let openSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
  tabStub = { opener: {}, name: "", close: vi.fn(), location: { replace: vi.fn() } };
  openSpy = vi.spyOn(window, "open").mockReturnValue(tabStub as unknown as Window);
});
afterEach(() => openSpy.mockRestore());

const vmRun = { ...run, machine_kind: "linux_vm" } as unknown as RunDetail;

describe("NeedsYouBanner", () => {
  it("a Linux VM run's takeover is the desktop, not the terminal (UXR-16)", () => {
    mount([request()], { ...run, machine_kind: "linux_vm" } as unknown as RunDetail);
    const b = screen.getByTestId("needs-you-banner");
    expect(within(b).getByText("Asked 2 min ago. The agent waits until you take over or decline.")).toBeInTheDocument();
    expect(within(b).queryByTestId("needs-you-mac-view-only")).toBeNull();
  });

  it("a macOS VM run whose desktop is view only keeps the terminal and says why (UXR-16)", () => {
    mount([request()], { ...run, machine_kind: "macos_vm", macos: { desktop: "observe" } } as unknown as RunDetail);
    const b = screen.getByTestId("needs-you-banner");
    expect(within(b).getByText(/Taking over opens the terminal/)).toBeInTheDocument();
    expect(within(b).getByTestId("needs-you-mac-view-only").textContent).toBe(
      "This Mac's desktop is view only in the browser. Take over in the terminal, or open a window on your Mac.",
    );
  });

  it("a macOS VM run whose Mac allows control takes over on the desktop (UXR-16)", () => {
    mount([request()], { ...run, machine_kind: "macos_vm", macos: { desktop: "control" } } as unknown as RunDetail);
    const b = screen.getByTestId("needs-you-banner");
    expect(within(b).queryByTestId("needs-you-mac-view-only")).toBeNull();
    expect(within(b).queryByText(/opens the terminal/)).toBeNull();
  });

  it("a snoozed request keeps the agent's reason on one line and Decline (UXR-51)", async () => {
    window.localStorage.setItem("wardyn.needs-you.snooze.ap-1", String(Date.now() + 600_000));
    api.decline.mockResolvedValue(request({ state: "DENIED" }));
    const cb = mount([request()]);
    const bar = screen.getByTestId("needs-you-snoozed");
    const line = within(bar).getByTestId("needs-you-snoozed-reason");
    expect(line.textContent).toBe(REASON);
    expect(line.className).toContain("truncate");
    await userEvent.click(within(bar).getByRole("button", { name: NEEDS_YOU.banner.decline }));
    expect(api.decline).toHaveBeenCalledWith("ap-1");
    expect(cb.onChanged).toHaveBeenCalled();
  });

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

  it("a background run offers no Take over, in the banner, the in-use row or the snoozed line", async () => {
    const bg = { ...run, experience: "background" } as unknown as RunDetail;
    mount([request()], bg);
    const b = screen.getByTestId("needs-you-banner");
    expect(within(b).queryByRole("button", { name: NEEDS_YOU.banner.takeOver })).toBeNull();
    expect(within(b).getByRole("button", { name: new RegExp(NEEDS_YOU.banner.decline) })).toBeInTheDocument();
    cleanup();
    mount([request({ state: "APPROVED", takeover_holder: "someone-else", decided_at: new Date().toISOString() })], bg);
    expect(screen.queryByRole("button", { name: NEEDS_YOU.banner.takeOver })).toBeNull();
    cleanup();
    window.localStorage.setItem("wardyn.needs-you.snooze.ap-1", String(Date.now() + 600_000));
    mount([request()], bg);
    expect(screen.getByTestId("needs-you-snoozed")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: NEEDS_YOU.banner.takeOver })).toBeNull();
    expect(api.takeOver).not.toHaveBeenCalled();
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

  it("a run whose policy set recording to Off says Recording off", () => {
    mount([request({ state: "APPROVED", decided_at: new Date().toISOString(), takeover_holder: "dev-sub" })], {
      ...run,
      not_recorded: "policy_off",
    } as RunDetail);
    expect(screen.getByTestId("needs-you-recording").textContent).toBe(NEEDS_YOU.strip.off);
  });

  it("a run whose runner declined to record still shows the takeover as recorded", () => {
    mount([request({ state: "APPROVED", decided_at: new Date().toISOString(), takeover_holder: "dev-sub" })], {
      ...run,
      not_recorded: "runner_declined",
    } as RunDetail);
    expect(screen.queryByTestId("needs-you-recording")).not.toBeInTheDocument();
  });

  it("a harness login run, which is never recorded, says Recording off", () => {
    mount([request({ state: "APPROVED", decided_at: new Date().toISOString(), takeover_holder: "dev-sub" })], {
      ...run,
      task: "harness login",
    } as RunDetail);
    expect(screen.getByTestId("needs-you-recording").textContent).toBe(NEEDS_YOU.strip.off);
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

  // The copy follows the server's answer where there is one; the run page's guess is only the fallback.
  it("says terminal for a VM run when the approval carries the server's terminal surface", () => {
    mount([{ ...request(), surface: "terminal" } as ApprovalRequest], vmRun);
    expect(within(screen.getByTestId("needs-you-banner")).getByText(NEEDS_YOU.banner.asked("2 min ago", "terminal"))).toBeInTheDocument();
  });

  it("keeps the VM guess (desktop) when the approval carries no surface", () => {
    mount([request()], vmRun);
    expect(within(screen.getByTestId("needs-you-banner")).getByText(NEEDS_YOU.banner.asked("2 min ago", "desktop"))).toBeInTheDocument();
  });

  it("after a takeover the server answered terminal for, the strip says terminal though the run page guessed desktop", async () => {
    api.takeOver.mockResolvedValue({ ...request({ state: "APPROVED" }), surface: "terminal" });
    const cb = { onChanged: vi.fn(), onOpenTerminal: vi.fn(), onGoAudit: vi.fn() };
    const view = render(<NeedsYouBanner run={vmRun} approvals={[request()]} {...cb} />);
    await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.takeOver }));
    expect(cb.onOpenTerminal).toHaveBeenCalled();
    view.rerender(<NeedsYouBanner run={vmRun} approvals={[request({ state: "APPROVED", takeover_holder: "dev-sub" })]} {...cb} />);
    expect(screen.getByText(NEEDS_YOU.strip.control("terminal"))).toBeInTheDocument();
  });

  describe("Take over opens the desktop tab inside the click", () => {
    it("opens a named blank tab before the takeover request is answered, and sends the app into it", async () => {
      let answer: (v: unknown) => void = () => {};
      api.takeOver.mockReturnValue(new Promise((r) => (answer = r)));
      mount([request()], vmRun);
      await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.takeOver }));
      expect(openSpy).toHaveBeenCalledTimes(1);
      expect(openSpy.mock.calls[0][0]).toBe("");
      expect(openSpy.mock.calls[0][1]).toMatch(/^wardyn-enter-/);
      expect(tabStub.opener).toBeNull();
      expect(api.takeOver).toHaveBeenCalledTimes(1);
      expect(tabStub.close).not.toHaveBeenCalled();
      answer({ ...request({ state: "APPROVED" }), surface: "terminal" });
    });

    it("closes the blank tab when the server puts the takeover on the terminal", async () => {
      api.takeOver.mockResolvedValue({ ...request({ state: "APPROVED" }), surface: "terminal" });
      const cb = mount([request()], vmRun);
      await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.takeOver }));
      expect(tabStub.close).toHaveBeenCalled();
      expect(cb.onOpenTerminal).toHaveBeenCalled();
    });

    it("closes the blank tab when the takeover fails", async () => {
      api.takeOver.mockRejectedValue(new Error("not now"));
      mount([request()], vmRun);
      await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.takeOver }));
      expect(tabStub.close).toHaveBeenCalled();
      expect(toastError).toHaveBeenCalledWith(NEEDS_YOU.failed.takeOver, { description: "not now" });
    });

    it("says the browser blocked the tab, and takes nothing over, when window.open returns null", async () => {
      openSpy.mockReturnValue(null);
      mount([request()], vmRun);
      await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.takeOver }));
      expect(toastError).toHaveBeenCalledWith(NEEDS_YOU.failed.takeOver, { description: RUN_SSH.POPUP_BLOCKED });
      expect(api.takeOver).not.toHaveBeenCalled();
    });

    it("opens no tab for a run whose takeover is the terminal", async () => {
      api.takeOver.mockResolvedValue({ ...request({ state: "APPROVED" }), surface: "terminal" });
      mount([request()]);
      await userEvent.click(screen.getByRole("button", { name: NEEDS_YOU.banner.takeOver }));
      expect(openSpy).not.toHaveBeenCalled();
    });
  });
});
