/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { MAC_SETTINGS } from "../mac-settings";
import { MAC_WINDOW } from "../mac-window";
import { NR_WINDOW } from "./window";

// The window on the Mac's strings (boards WIN-1 and WIN-2, as approved), character for character. Every key of the
// two modules has a row, so a new string can't ship without one. Editing one is a copy change and needs the mock's
// sign-off.
const NR: Record<keyof typeof NR_WINDOW, [id: string, actual: string, canon: string]> = {
  GROUP: ["W2-01", NR_WINDOW.GROUP, "Window"],
  OPTION: ["W2-02", NR_WINDOW.OPTION, "Open in a window on my Mac"],
  ONE_MAC: ["W2-03", NR_WINDOW.ONE_MAC("{runner}"), "Opens a window on {runner} as the run starts, where you can type and click. You can also open one later from the run page."],
  BE_THERE: ["W2-04", NR_WINDOW.BE_THERE("{runner}"), "Be at {runner} with its screen unlocked when the run starts. If the window can't open, the run still starts and its page says why."],
  SEVERAL_MACS: ["W2-05", NR_WINDOW.SEVERAL_MACS, "Opens only if the run lands on a Mac you registered. On any other Mac the run starts without a window, and its page says why."],
  CLEARED: ["W2-06", NR_WINDOW.CLEARED, "Window choice cleared."],
  REVIEW_ONE_MAC: ["W2-07", NR_WINDOW.REVIEW_ONE_MAC("{runner}"), "Window on {runner} when the run starts"],
  REVIEW_SEVERAL_MACS: ["W2-08", NR_WINDOW.REVIEW_SEVERAL_MACS, "Window on your Mac when the run starts, if it lands on one you registered"],
  OS_WINDOWS_LATER: ["V-05R", NR_WINDOW.OS_WINDOWS_LATER, "Windows virtual machines aren't available yet."],
  OS_LATER: ["V-06R", NR_WINDOW.OS_LATER, "macOS virtual machines are turned off in this organisation. Windows virtual machines aren't available yet."],
  VIEW_ONLY_WINDOW_ON: ["E-02 on", NR_WINDOW.VIEW_ONLY_WINDOW_ON, "On macOS 26 the browser shows this desktop view only. Use the terminal for commands, or open a window on the Mac to interact with the desktop."],
  VIEW_ONLY_WINDOW_OFF: ["E-02 off", NR_WINDOW.VIEW_ONLY_WINDOW_OFF, "On macOS 26 the browser shows this desktop view only. Use the terminal for commands."],
};

const RUN_PAGE: Record<keyof typeof MAC_WINDOW, [id: string, actual: string, canon: string]> = {
  open: ["W-01", MAC_WINDOW.open("{runner}"), "Open window on {runner}"],
  openCompact: ["W-01 compact", MAC_WINDOW.openCompact, "Open Mac window"],
  opensOn: ["W-01 line", MAC_WINDOW.opensOn("{runner}"), "Opens on {runner}"],
  explain: ["W-02", MAC_WINDOW.explain("{runner}"), "Opens a window on {runner}'s own screen. What you type there goes into this run. Close the window to hand back; the run keeps going."],
  waiting: ["W-03", MAC_WINDOW.waiting("{runner}"), "Waiting for {runner} to show the window. This can take up to 30 seconds."],
  openChip: ["W-04", MAC_WINDOW.openChip, "Window open"],
  handBack: ["W-05", MAC_WINDOW.handBack("{runner}"), "Close the window on {runner} to hand back. The run keeps going."],
  viewOnlyWhileOpen: ["W-06", MAC_WINDOW.viewOnlyWhileOpen, "While the window is open, the desktop on this page is view only."],
  closedPerson: ["W-08", MAC_WINDOW.closedPerson, "You closed the window. The run is still going."],
  closedLocked: ["W-09", MAC_WINDOW.closedLocked("{runner}"), "The window closed because {runner}'s screen locked. Unlock it and open the window again. The run is still going."],
  closedRunEnded: ["W-10", MAC_WINDOW.closedRunEnded, "The window closed because the run ended."],
  closedFailed: ["W-11", MAC_WINDOW.closedFailed, "The window closed unexpectedly. The run is still going. You can open it again."],
  notOpened: ["W2-12", MAC_WINDOW.notOpened, "The window didn't open when the run started."],
  background: ["W-12", MAC_WINDOW.background, "This is a background run. What you type in the window goes into it."],
  watchLine: ["watch line", MAC_WINDOW.watchLine, "In the browser you can watch, not type or click. To work in it, open the window on the Mac."],
  thisMac: ["runner fallback", MAC_WINDOW.thisMac, "the run's Mac"],
};

describe("window on the Mac canon", () => {
  it.each(Object.values(NR))("New Run %s", (_id, actual, canon) => expect(actual).toBe(canon));
  it.each(Object.values(RUN_PAGE))("run page %s", (_id, actual, canon) => expect(actual).toBe(canon));
  it("every key has a row", () => {
    expect(Object.keys(NR).sort()).toEqual(Object.keys(NR_WINDOW).sort());
    expect(Object.keys(RUN_PAGE).sort()).toEqual(Object.keys(MAC_WINDOW).sort());
  });
  it("Settings W2-09 and W2-10", () => {
    expect(MAC_SETTINGS.WINDOW_LABEL).toBe("Allow a window on the Mac for macOS VM runs");
    expect(MAC_SETTINGS.WINDOW_HINT.startsWith("Off by default.")).toBe(true);
  });
});
