/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop tile's window on the Mac (board WIN-1, and WIN-2's launch line W2-12): the button, its wait, the open
// window, the refusals in the server's words, the after-click errors, the last close, and the runs that have none.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail, RunMacWindow } from "../../lib/types";
import type { MacOSRunFacts } from "./run-detail/macos-run-facts";

const healthMock = vi.fn();
const openMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
vi.mock("../../lib/api/runs", () => ({ runs: { getPolicy: () => new Promise(() => {}), attachTicket: vi.fn(), watchTicket: vi.fn(), openMacWindow: (...a: unknown[]) => openMock(...a) } }));
vi.mock("../../lib/api/desktop-sign-ins", () => ({ desktopSignIns: { get: vi.fn(async () => ({ store: "run_disk", paused: false, can_pause: true })), pause: vi.fn(), resume: vi.fn() } }));

import { HttpError } from "../../lib/api/core";
import { DesktopCard } from "./run-detail-desktop";
import { ASKED_MS } from "./run-detail/mac-window-section";
import { OperatorProvider } from "../wardyn/operator-context";
import { UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { MAC_WINDOW } from "../wardyn/copy/mac-window";
import { aheadByHours } from "../../lib/test-clock";

const OWNER = "alice@example.com";
const MAC = "Maya's MacBook";
const run: RunDetail = {
  id: "33333333-3333-3333-3333-333333333333",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/33333333-3333-3333-3333-333333333333",
  runner_target: "docker",
  placement: "",
  interactive: true,
  machine_kind: "macos_vm",
  runner_name: MAC,
};
const OBSERVE: MacOSRunFacts = { desktop: "observe" };

const card = (w: RunMacWindow | undefined, over: Partial<RunDetail> = {}) => (
  <MemoryRouter>
    <OperatorProvider operator={false} principal={OWNER}>
      <DesktopCard run={{ ...run, mac_window: w, ...over }} macOS={OBSERVE} />
    </OperatorProvider>
  </MemoryRouter>
);
const desk = () => screen.findByRole("region", { name: DESKTOP.title });
const tile = (region: HTMLElement) => region.parentElement!.parentElement!;
const windowButton = (d: HTMLElement) => within(d).getByRole("button", { name: new RegExp(`^(${MAC_WINDOW.open(MAC)}|${MAC_WINDOW.openCompact})`) });

beforeEach(() => {
  healthMock.mockReset().mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
  openMock.mockReset();
});

describe("DesktopCard: the window on the Mac", () => {
  it("available: the window button first, named for the Mac and compact on a phone, the Opens-on line and W-02, then Watch desktop", async () => {
    render(card({ state: "open" }));
    const d = await desk();
    const button = windowButton(d);
    expect(button).toBeEnabled();
    expect(within(button).getByText(MAC_WINDOW.open(MAC))).toHaveClass("hidden", "sm:inline");
    expect(within(button).getByText(MAC_WINDOW.openCompact)).toHaveClass("sm:hidden");
    expect(within(d).getByText(MAC_WINDOW.opensOn(MAC))).toBeInTheDocument();
    expect(within(d).getByText(MAC_WINDOW.explain(MAC))).toBeInTheDocument();
    const watch = within(d).getByRole("button", { name: DESKTOP.watch });
    // Keyboard order follows the DOM: the window button, then Watch desktop.
    expect(button.compareDocumentPosition(watch) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(d).getByText(MAC_WINDOW.watchLine)).toBeInTheDocument();
    expect(within(d).getByText(UI_APPS_LANE.newTab)).toBeInTheDocument();
  });

  it("a run read with no Mac name: the compact label only, and no Opens-on line", async () => {
    render(card({ state: "open" }, { runner_name: undefined }));
    const d = await desk();
    expect(within(d).getByRole("button", { name: MAC_WINDOW.openCompact })).toBeEnabled();
    expect(within(d).queryByText(/^Opens on /)).toBeNull();
    expect(within(d).getByText(MAC_WINDOW.explain(MAC_WINDOW.thisMac))).toBeInTheDocument();
  });

  it("opening: the button waits with W-03 until the run read says the window is open, then the tile says so", async () => {
    let done: () => void = () => {};
    openMock.mockImplementation(() => new Promise<void>((ok) => { done = ok; }));
    const { rerender } = render(card({ state: "open" }));
    const d = await desk();
    await userEvent.click(windowButton(d));
    expect(openMock).toHaveBeenCalledWith(run.id);
    expect(within(d).getByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeDisabled();
    expect(within(d).getByText(MAC_WINDOW.waiting(MAC))).toBeInTheDocument();
    done();
    // Answered, but the read has not changed yet: still waiting.
    await waitFor(() => expect(openMock).toHaveBeenCalledTimes(1));
    expect(within(d).getByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeDisabled();
    rerender(card({ state: "window_open", message: `This run's window is already open on ${MAC}.` }));
    const open = await desk();
    expect(within(tile(open)).getByText(MAC_WINDOW.openChip)).toBeInTheDocument();
    expect(within(tile(open)).queryByText(DESKTOP.viewOnlyChip)).toBeNull();
    expect(within(open).getByText(`This run's window is already open on ${MAC}.`)).toBeInTheDocument();
    expect(within(open).getByText(MAC_WINDOW.handBack(MAC))).toBeInTheDocument();
    expect(within(open).getByText(MAC_WINDOW.viewOnlyWhileOpen)).toBeInTheDocument();
    expect(within(open).queryByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeNull();
    expect(within(open).getByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
  });

  it("a window that opened and closed between two reads: the newer last close frees the button, and it opens again", async () => {
    openMock.mockResolvedValue({});
    const before = aheadByHours(-0.5);
    const { rerender } = render(card({ state: "open", last_close: { reason: "person", at: before } }));
    const d = await desk();
    await userEvent.click(windowButton(d));
    await waitFor(() => expect(within(d).getByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeDisabled());
    // The same read again: still waiting.
    rerender(card({ state: "open", last_close: { reason: "person", at: before } }));
    expect(within(await desk()).getByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeDisabled();
    // Open both times, but a newer close: the window came and went.
    rerender(card({ state: "open", last_close: { reason: "host_locked", at: aheadByHours(-0.01) } }));
    const after = await desk();
    expect(windowButton(after)).toBeEnabled();
    expect(within(after).getByText(MAC_WINDOW.closedLocked(MAC))).toBeInTheDocument();
    await userEvent.click(windowButton(after));
    expect(openMock).toHaveBeenCalledTimes(2);
  });

  it("the wait belongs to the run it was asked on: another run's tile offers its own button", async () => {
    openMock.mockResolvedValue({});
    const { rerender } = render(card({ state: "open" }));
    await userEvent.click(windowButton(await desk()));
    await waitFor(async () => expect(within(await desk()).getByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeDisabled());
    rerender(card({ state: "open" }, { id: "44444444-4444-4444-4444-444444444444" }));
    expect(windowButton(await desk())).toBeEnabled();
  });

  it("a read that never shows the window: the button is offered again after the wait", async () => {
    openMock.mockResolvedValue({});
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      render(card({ state: "open" }));
      const d = await desk();
      await user.click(windowButton(d));
      await waitFor(() => expect(within(d).getByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeDisabled());
      await act(async () => { vi.advanceTimersByTime(ASKED_MS - 1000); });
      expect(within(d).getByRole("button", { name: UI_APPS_LANE.ctaBusy })).toBeDisabled();
      await act(async () => { vi.advanceTimersByTime(1000); });
      expect(windowButton(d)).toBeEnabled();
    } finally {
      vi.useRealTimers();
    }
  });

  it.each([
    ["not at the Mac's screen", 409, `The window didn't open: you aren't signed in at ${MAC}'s screen, or it is locked. Unlock it and open the window again.`],
    ["the lock state unknown", 409, "Wardyn can't tell whether this Mac's screen is locked, so the window stays closed."],
    ["the Mac could not open it", 502, "Wardyn couldn't open the window on the run's Mac. The run goes on; try again."],
    ["a browser desktop kept control", 409, "A browser desktop that has control of this run didn't close in time, so Wardyn can't open the window. Close it and open the window again."],
    ["the session revoked", 403, "This session is no longer authorized, so Wardyn won't open a window. Sign in again."],
  ])("after the click, %s: the server's sentence as an alert, and the button back", async (_name, status, message) => {
    openMock.mockRejectedValue(new HttpError(status, message, "mac_window_x"));
    render(card({ state: "open" }));
    const d = await desk();
    await userEvent.click(windowButton(d));
    expect(await within(d).findByRole("alert")).toHaveTextContent(message);
    expect(windowButton(d)).toBeEnabled();
  });

  it.each([
    ["preparing", "This run is still being prepared; the window opens when preparation finishes."],
    ["paused", "This run is paused. You can open its window once it is running again."],
  ] as const)("%s: the button stays, disabled, with the server's reason", async (state, message) => {
    render(card({ state, message }));
    const d = await desk();
    expect(windowButton(d)).toBeDisabled();
    expect(within(d).getByText(message)).toBeInTheDocument();
    expect(within(d).queryByText(MAC_WINDOW.explain(MAC))).toBeNull();
  });

  it.each([
    ["site_off", "Opening a window onto a macOS VM run is turned off for this install. An admin can turn it on in Settings, under macOS virtual machines, or set macos_vm.allow_attended_window in the site config."],
    ["governance", "Your organisation's governance profile doesn't allow an interactive desktop on this run."],
    ["sign_ins_off", "Your admin doesn't allow sign-ins in a desktop while Claude Code runs, and a macOS desktop can't block them yet."],
    ["not_your_mac", `Wardyn opens a window only on a Mac whose runner you registered. This run is on ${MAC}, so Wardyn can't tell who is at its keyboard. Watch its desktop from the run page instead.`],
    ["unrecorded", "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead."],
    ["offline", "This Mac's runner is offline, so Wardyn can't open a window on it."],
    ["runner_too_old", "This Mac's runner can't open a window. Upgrade it to this server's version."],
    ["not_here", "Wardyn couldn't reach this Mac's runner from here. Try again in a moment."],
  ] as const)("refused (%s): the server's sentence under the watch line, and no window button", async (state, message) => {
    render(card({ state, message }));
    const d = await desk();
    const watchLine = within(d).getByText(DESKTOP.observeOnlyMacOS);
    const refusal = within(d).getByText(message);
    expect(watchLine.compareDocumentPosition(refusal) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(d).queryByRole("button", { name: new RegExp(MAC_WINDOW.openCompact) })).toBeNull();
    expect(within(d).getByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
  });

  it("unconfirmed with no sentence, and a person who isn't the run's owner: nothing about a window", async () => {
    for (const w of [{ state: "unconfirmed" }, { state: "not_run_owner" }] as RunMacWindow[]) {
      const { unmount } = render(card(w));
      const d = await desk();
      expect(within(d).queryByTestId("mac-window")).toBeNull();
      expect(within(d).queryByTestId("mac-window-refusal")?.textContent ?? "").toBe("");
      expect(within(d).getByText(DESKTOP.observeOnlyMacOS)).toBeInTheDocument();
      unmount();
    }
  });

  it.each([
    ["person", MAC_WINDOW.closedPerson],
    ["host_locked", MAC_WINDOW.closedLocked(MAC)],
    ["run_ended", MAC_WINDOW.closedRunEnded],
    ["failed", MAC_WINDOW.closedFailed],
  ] as const)("the last close (%s) is said under the button, which is back", async (reason, text) => {
    render(card({ state: "open", last_close: { reason, at: aheadByHours(-0.1) } }));
    const d = await desk();
    expect(windowButton(d)).toBeEnabled();
    expect(within(d).getByText(text)).toBeInTheDocument();
  });

  it("a window refused at launch (W2-12): the run page says it didn't open, then the server's sentence", async () => {
    const why = `The window didn't open: you aren't signed in at ${MAC}'s screen, or it is locked. Unlock it and open the window again.`;
    render(card({ state: "open", last_close: { reason: "not_opened", message: why, at: aheadByHours(-0.1) } }));
    const d = await desk();
    expect(within(d).getByText(`${MAC_WINDOW.notOpened} ${why}`)).toBeInTheDocument();
    expect(windowButton(d)).toBeEnabled();
  });

  it("a window refused at launch on a run whose window is now refused: the refusal, then the launch line", async () => {
    const off = "Opening a window onto a macOS VM run is turned off for this install. An admin can turn it on in Settings, under macOS virtual machines, or set macos_vm.allow_attended_window in the site config.";
    render(card({ state: "site_off", message: off, last_close: { reason: "not_opened", message: off, at: aheadByHours(-0.1) } }));
    const d = await desk();
    expect(within(d).getByText(off)).toBeInTheDocument();
    expect(within(d).getByText(`${MAC_WINDOW.notOpened} ${off}`)).toBeInTheDocument();
  });

  it("a background run: the window is a takeover, and W-12 says what typing does", async () => {
    render(card({ state: "open" }, { experience: "background", interactive: false, background_watch: { state: "open" } }));
    const d = await desk();
    expect(windowButton(d)).toBeEnabled();
    expect(within(d).getByText(MAC_WINDOW.background)).toBeInTheDocument();
  });

  it("a Linux VM run: no window button and nothing about a window", async () => {
    render(card(undefined, { machine_kind: "linux_vm" }));
    const d = await desk();
    expect(within(d).queryByTestId("mac-window")).toBeNull();
    expect(d.textContent).not.toMatch(/window on/i);
  });
});
