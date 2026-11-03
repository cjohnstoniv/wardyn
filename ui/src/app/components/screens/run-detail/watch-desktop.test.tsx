/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hero's Watch desktop: a refused ticket's error wraps in the bar and is not cut to one line behind a title attribute.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { makeMacOSVMRun } from "../../../../test/factories";

vi.mock("../../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } }) },
}));
const { LONG } = vi.hoisted(() => ({ LONG: "the ticket could not be minted because the run is between states and the gateway asked for another try in a moment" }));
vi.mock("../run-detail-ssh", () => ({ DESKTOP_APP: { name: "__desktop" }, enterUIApp: vi.fn().mockRejectedValue(new Error(LONG)) }));

import { WatchDesktopButton } from "./watch-desktop";
import { DESKTOP } from "../../wardyn/copy/desktop";

describe("WatchDesktopButton", () => {
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
