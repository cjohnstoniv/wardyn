/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hero's Watch desktop: a refused ticket's error wraps in the bar and is not cut to one line behind a title attribute.
import { beforeEach, describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { makeMacOSVMRun } from "../../../../test/factories";

vi.mock("../../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } }) },
}));
const { LONG } = vi.hoisted(() => ({ LONG: "the ticket could not be minted because the run is between states and the gateway asked for another try in a moment" }));
const ssh = vi.hoisted(() => ({ openEnterTab: vi.fn(), enterUIApp: vi.fn() }));
vi.mock("../run-detail-ssh", () => ({ DESKTOP_APP: { name: "__desktop" }, ...ssh }));

import { WatchDesktopButton } from "./watch-desktop";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { RUN_SSH } from "../../wardyn/copy";

const tab = { close: vi.fn() } as unknown as Window;
beforeEach(() => {
  ssh.openEnterTab.mockReset().mockReturnValue(tab);
  ssh.enterUIApp.mockReset().mockRejectedValue(new Error(LONG));
});

describe("WatchDesktopButton", () => {
  it("opens the tab inside the click and hands it to enterUIApp", async () => {
    ssh.enterUIApp.mockResolvedValue(undefined);
    render(<WatchDesktopButton run={makeMacOSVMRun({ state: "RUNNING", interactive: true })} />);
    fireEvent.click(await screen.findByRole("button", { name: DESKTOP.watch }));
    expect(ssh.openEnterTab).toHaveBeenCalledTimes(1);
    expect(ssh.enterUIApp).toHaveBeenCalledTimes(1);
    expect(ssh.enterUIApp.mock.calls[0][3]).toEqual({ tab });
    expect(ssh.openEnterTab.mock.invocationCallOrder[0]).toBeLessThan(ssh.enterUIApp.mock.invocationCallOrder[0]);
  });

  it("says the browser blocked the tab, and enters nothing, when no tab opens", async () => {
    ssh.openEnterTab.mockReturnValue(null);
    render(<WatchDesktopButton run={makeMacOSVMRun({ state: "RUNNING", interactive: true })} />);
    fireEvent.click(await screen.findByRole("button", { name: DESKTOP.watch }));
    expect(await screen.findByRole("alert")).toHaveTextContent(RUN_SSH.POPUP_BLOCKED);
    expect(ssh.enterUIApp).not.toHaveBeenCalled();
  });

  it("shows the whole error, wrapped, with no one-line cut and no title-only text", async () => {
    render(<WatchDesktopButton run={makeMacOSVMRun({ state: "RUNNING", interactive: true })} />);
    fireEvent.click(await screen.findByRole("button", { name: DESKTOP.watch }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(LONG);
    expect(alert.className).not.toContain("truncate");
    expect(alert.className).toContain("break-words");
    expect(alert.hasAttribute("title")).toBe(false);
  });
});
