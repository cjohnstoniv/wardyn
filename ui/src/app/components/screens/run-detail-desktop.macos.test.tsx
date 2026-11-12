/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop tile for a macOS VM run.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";
import type { MacOSRunFacts } from "./run-detail/macos-run-facts";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
vi.mock("../../lib/api/runs", () => ({ runs: { getPolicy: () => new Promise(() => {}), attachTicket: vi.fn() } }));
const signInsGet = vi.fn();
const signInsPause = vi.fn();
vi.mock("../../lib/api/desktop-sign-ins", () => ({
  desktopSignIns: { get: (...a: unknown[]) => signInsGet(...a), pause: (...a: unknown[]) => signInsPause(...a), resume: vi.fn() },
}));

import { DesktopCard } from "./run-detail-desktop";
import { OperatorProvider } from "../wardyn/operator-context";
import { UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { MAC_WINDOW } from "../wardyn/copy/mac-window";
import { RUN_VM_PAGE } from "../wardyn/copy/run-cockpit";
import { DESKTOP_FILES } from "../wardyn/copy";
import { aheadByHours } from "../../lib/test-clock";
import { shortTime } from "../../lib/format";

const OWNER = "alice@example.com";
const run: RunDetail = {
  id: "22222222-2222-2222-2222-222222222222",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/22222222-2222-2222-2222-222222222222",
  runner_target: "docker",
  placement: "",
  interactive: true,
  machine_kind: "macos_vm",
};
const gatewayOn = { status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } };
const runDisk = { store: "run_disk", paused: false, can_pause: true, agent: "Claude Code" };

const renderCard = (macOS?: MacOSRunFacts, over: Partial<RunDetail> = {}) =>
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal={OWNER}>
        <DesktopCard run={{ ...run, ...over }} macOS={macOS} />
      </OperatorProvider>
    </MemoryRouter>,
  );
const desk = () => screen.findByRole("region", { name: DESKTOP.title });

// The chip is in the tile's header, a sibling of the region that holds the body.
const tile = (region: HTMLElement) => region.parentElement!.parentElement!;

beforeEach(() => {
  healthMock.mockReset();
  signInsGet.mockReset();
  signInsPause.mockReset();
  healthMock.mockResolvedValue(gatewayOn);
  signInsGet.mockResolvedValue(runDisk);
});

describe("DesktopCard: macOS desktop", () => {
  it("Ready, the macOS kind line, the same Open and pause buttons, and the run-disk note last", async () => {
    renderCard();
    const d = await desk();
    expect(await within(d).findByRole("button", { name: DESKTOP.pauseToSignIn })).toBeEnabled();
    expect(within(tile(d)).getByText(DESKTOP.ready)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kindMacOS)).toBeInTheDocument();
    expect(within(d).queryByText(DESKTOP.kind)).toBeNull();
    expect(within(d).getByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    expect(within(d).getByText(UI_APPS_LANE.newTab)).toBeInTheDocument();
    expect(d.lastElementChild).toBe(within(d).getByText(DESKTOP.signInsNoteRunDisk));
    expect(within(d).queryByText(DESKTOP.signInsNote)).toBeNull();
  });

  it("while paused the chip says Agent paused, with today's pause words and the macOS note", async () => {
    signInsGet.mockResolvedValue({ ...runDisk, paused: true });
    renderCard();
    const d = await desk();
    expect(await within(tile(d)).findByText(DESKTOP.agentPaused)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.pausedNote)).toBeInTheDocument();
    expect(within(d).getByRole("button", { name: DESKTOP.resume })).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.signInsNoteRunDisk)).toBeInTheDocument();
  });

  it.each<[from: string, facts: MacOSRunFacts | undefined, over: Partial<RunDetail>]>([
    ["a prop", { desktop: "observe" }, {}],
    ["the run's own macos field", undefined, { macos: { desktop: "observe" } }],
  ])("board F-block: a view-only desktop says so and offers Watch desktop, with no sign-ins (from %s)", async (_from, facts, over) => {
    renderCard(facts, over);
    const d = await desk();
    expect(await within(tile(d)).findByText(DESKTOP.viewOnlyChip)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kindMacOS)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.observeOnlyMacOS)).toBeInTheDocument();
    expect(within(d).getAllByRole("button").map((b) => b.textContent)).toEqual([DESKTOP.watch]);
    expect(within(d).queryByText(DESKTOP.off)).toBeNull();
    expect(signInsGet).not.toHaveBeenCalled();
  });

  it("board F-block: a view-only desktop with the gateway off is Off, with no button", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: false } });
    renderCard({ desktop: "observe" });
    const d = await desk();
    expect(await within(tile(d)).findByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.observeOnlyMacOS)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
    expect(within(d).getByText(/Off on this deployment/)).toBeInTheDocument();
  });

  it("a Mac whose Screen Sharing can be controlled keeps the Open desktop", async () => {
    renderCard(undefined, { macos: { desktop: "control" } });
    const d = await desk();
    expect(await within(d).findByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    expect(within(d).queryByText(DESKTOP.observeOnlyMacOS)).toBeNull();
  });

  it.each<[frame: string, disk: NonNullable<MacOSRunFacts["disk"]>, line: string]>([
    ["9", { state: "removing" }, DESKTOP.diskRemoving],
    ["10", { state: "removed", at: "14:32" }, DESKTOP.diskRemoved("14:32")],
    ["11", { state: "failed", reason: DESKTOP.diskReasonInUse }, `${DESKTOP.diskRemoveFailed(DESKTOP.diskReasonInUse)} ${RUN_VM_PAGE.diskKeptNextStep}`],
  ])("frame %s: at the run's end the block is Off and carries the disk line alone", async (_frame, disk, line) => {
    renderCard({ disk }, { state: "COMPLETED" });
    const d = await desk();
    expect(within(tile(d)).getByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kindMacOS)).toBeInTheDocument();
    expect(within(d).getByText(line)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
  });

  const removedAt = aheadByHours(-1);
  it.each<[frame: string, disk: NonNullable<RunDetail["macos_disk"]>, line: string]>([
    ["10", { state: "removed", at: removedAt }, DESKTOP.diskRemoved(shortTime(removedAt))],
    ["11", { state: "kept", reason: "helper_alive" }, `${DESKTOP.diskRemoveFailed(DESKTOP.diskReasonInUse)} ${RUN_VM_PAGE.diskKeptNextStep}`],
  ])("frame %s: the disk line comes from the run's own macos_disk, with no prop", async (_frame, disk, line) => {
    renderCard(undefined, { state: "COMPLETED", macos_disk: disk });
    const d = await desk();
    expect(within(tile(d)).getByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(d).getByText(line)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
  });

  // UXR-34: a kept disk names its next step.
  it("a disk that could not be removed says it still takes room and who to tell", async () => {
    renderCard({ disk: { state: "failed", reason: DESKTOP.diskReasonUnknown } }, { state: "COMPLETED" });
    const d = await desk();
    expect(within(d).getByRole("alert")).toHaveTextContent(
      "The macOS VM's disk could not be removed from this Mac: the Mac didn't say why. It still takes room on this Mac. Tell the Mac's owner so they can remove it.",
    );
  });

  // UXR-6: the view-only sentence says how to type and click: in the window, which has its own button now.
  it("on macOS 26 with a window available, the window button stands where the command was, and the watch line points to it", async () => {
    renderCard({ desktop: "observe" }, { mac_window: { state: "open" } });
    const d = await desk();
    expect(within(d).getByRole("button", { name: MAC_WINDOW.openCompact })).toBeEnabled();
    expect(within(d).getByText(MAC_WINDOW.watchLine)).toBeInTheDocument();
    expect(d.textContent).not.toContain("wardyn window");
    expect(within(d).queryByText(DESKTOP.observeOnlyMacOS)).toBeNull();
  });

  it("on macOS 26 with a window refused, the watch line stays and the server's own sentence sits under it", async () => {
    renderCard({ desktop: "observe" }, { mac_window: { state: "site_off", message: "Windows on the Mac are turned off for this install." } });
    const d = await desk();
    const watchLine = within(d).getByText(DESKTOP.observeOnlyMacOS);
    const refusal = within(d).getByText("Windows on the Mac are turned off for this install.");
    expect(watchLine.compareDocumentPosition(refusal) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(d).queryByRole("button", { name: MAC_WINDOW.openCompact })).toBeNull();
    expect(d.textContent).not.toContain("wardyn window");
  });

  // At Screen and actions the tile says once what the screen recording keeps; the window refusal sits under the
  // watch-only line, not in its place.
  it("says the screen is recorded while watched at Screen and actions, on a Linux VM too, and nothing at Terminal", async () => {
    const first = renderCard({ desktop: "observe" }, { recording_level: "screen_actions" });
    expect(within(await desk()).getByTestId("desktop-recorded-line")).toHaveTextContent("Recorded while you watch; not masked");
    first.unmount();
    const linux = renderCard(undefined, { recording_level: "screen_actions", machine_kind: "linux_vm" });
    expect(within(await desk()).getByTestId("desktop-recorded-line")).toHaveTextContent("Recorded while you watch; not masked");
    linux.unmount();
    const terminal = renderCard({ desktop: "observe" }, { recording_level: "terminal" });
    await desk();
    expect(screen.queryByTestId("desktop-recorded-line")).toBeNull();
    terminal.unmount();
    // A macOS run marked runner_declined (its agent's output) is still recorded on screen; a Linux VM so marked is not.
    const declined = renderCard({ desktop: "observe" }, { recording_level: "screen_actions", not_recorded: "runner_declined" });
    expect(within(await desk()).getByTestId("desktop-recorded-line")).toBeInTheDocument();
    declined.unmount();
    renderCard(undefined, { recording_level: "screen_actions", machine_kind: "linux_vm", not_recorded: "runner_declined" });
    await desk();
    expect(screen.queryByTestId("desktop-recorded-line")).toBeNull();
  });

  it("says no frames are kept while the agent is paused for a sign-in", async () => {
    signInsGet.mockResolvedValue({ ...runDisk, paused: true });
    renderCard(undefined, { recording_level: "screen_actions", machine_kind: "linux_vm" });
    const d = await desk();
    expect(await within(d).findByText("Recording paused while you sign in.")).toBeInTheDocument();
    expect(within(d).queryByText("Recorded while you watch; not masked")).toBeNull();
  });

  it("an unrecorded window sentence stands under the macOS 26 watch-only line, and both stay", async () => {
    const sentence = "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead.";
    renderCard({ desktop: "observe" }, { recording_level: "screen_actions", mac_window: { state: "unrecorded", message: sentence } });
    const d = await desk();
    const watchOnly = within(d).getByText(DESKTOP.observeOnlyMacOS);
    const refusal = within(d).getByText(sentence);
    expect(watchOnly.compareDocumentPosition(refusal) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  // UXR-45: no View only chip beside the Off sentence.
  it("a background run with an open watch and the gateway off draws Off, not View only", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: false } });
    renderCard(undefined, { experience: "background", background_watch: { state: "open" } });
    const d = await desk();
    expect(await within(tile(d)).findByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(tile(d)).queryByText(DESKTOP.viewOnlyChip)).toBeNull();
  });

  // UXR-27: a /healthz that cannot be read is not "Off".
  it("a failed gateway check says so and Try again asks again", async () => {
    healthMock.mockResolvedValueOnce({});
    renderCard();
    const d = await desk();
    expect(await within(d).findByText(RUN_VM_PAGE.gatewayUnknown)).toBeInTheDocument();
    expect(d.textContent).not.toMatch(/Off on this deployment/);
    healthMock.mockResolvedValue(gatewayOn);
    within(d).getByRole("button", { name: RUN_VM_PAGE.tryAgain }).click();
    expect(await within(d).findByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    expect(within(d).queryByText(RUN_VM_PAGE.gatewayUnknown)).toBeNull();
    expect(healthMock).toHaveBeenCalledTimes(2);
  });

  // UXR-29/30 (render side): the clipboard strip is not claimed on a VM desktop.
  it("Files on a VM says copy and paste are not available, with no Allowed strip", async () => {
    const desktopFiles = await import("../../lib/api/desktop-files");
    vi.spyOn(desktopFiles.desktopFiles, "list").mockResolvedValue({
      folder: "~/Transfers",
      rules: { paste_in: "allowed", copy_out: "allowed", upload: "allowed", download: "allowed" },
      files: [],
    } as never);
    renderCard(undefined, { ui_apps: [{ name: "novnc", port: 6080 }] });
    const d = await desk();
    expect(await within(d).findByText(DESKTOP_FILES.clipboardVM)).toBeInTheDocument();
    expect(DESKTOP_FILES.clipboardVM).toBe("Copy and paste: not available on VM desktops yet");
    expect(within(d).queryByText(DESKTOP_FILES.pasteIn)).toBeNull();
  });
});
