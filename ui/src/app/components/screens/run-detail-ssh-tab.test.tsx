/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// enterUIApp with a tab the caller opened inside its own click (openEnterTab): no second tab, the form posts into the
// given one, and it is closed when the request fails.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const attachTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { attachTicket: (...a: unknown[]) => attachTicketMock(...a) },
}));

import { enterUIApp, openEnterTab } from "./run-detail-ssh";

let tabStub: { opener: unknown; name: string; close: ReturnType<typeof vi.fn>; location: { replace: ReturnType<typeof vi.fn> } };
let openSpy: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  attachTicketMock.mockReset();
  tabStub = { opener: {}, name: "", close: vi.fn(), location: { replace: vi.fn() } };
  openSpy = vi.spyOn(window, "open").mockReturnValue(tabStub as unknown as Window);
});
afterEach(() => openSpy.mockRestore());

describe("enterUIApp — a tab the caller opened inside its click", () => {
  const gateway = { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" };

  it("openEnterTab opens a named blank tab with no opener, or returns null when the browser blocks it", () => {
    const tab = openEnterTab();
    expect(tab).toBe(tabStub);
    expect(openSpy.mock.calls[0][0]).toBe("");
    expect(tabStub.opener).toBeNull();
    openSpy.mockReturnValue(null);
    expect(openEnterTab()).toBeNull();
  });

  it("opens no second tab, and posts the form into the given one", async () => {
    attachTicketMock.mockResolvedValue("tkt_x");
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    const tab = openEnterTab()!;
    openSpy.mockClear();
    await enterUIApp("run-1", "__desktop", gateway, { tab });
    expect(openSpy).not.toHaveBeenCalled();
    expect((submitSpy.mock.instances[0] as HTMLFormElement).target).toBe(tab.name);
    // A noopener form could make the browser open a second tab instead of reusing the named one.
    expect((submitSpy.mock.instances[0] as HTMLFormElement & { rel?: string }).rel).toBeUndefined();
    expect(tab.name).toMatch(/^wardyn-enter-/);
    submitSpy.mockRestore();
  });

  it("closes the given tab when the ticket cannot be minted", async () => {
    attachTicketMock.mockRejectedValue(new Error("no ticket"));
    const tab = openEnterTab()!;
    await expect(enterUIApp("run-1", "__desktop", gateway, { tab })).rejects.toThrow("no ticket");
    expect(tabStub.close).toHaveBeenCalled();
    expect(tab).toBe(tabStub);
  });

  it("ends quietly, with no form, no navigation and no new tab, when the person closed the tab while the ticket was pending", async () => {
    let release: (t: string) => void = () => {};
    attachTicketMock.mockReturnValue(new Promise<string>((r) => (release = r)));
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    const tab = openEnterTab()!;
    openSpy.mockClear();
    const entered = enterUIApp("run-1", "__desktop", gateway, { tab });
    Object.assign(tabStub, { closed: true });
    release("tkt_x");
    await expect(entered).resolves.toBeUndefined();
    expect(submitSpy).not.toHaveBeenCalled();
    expect(openSpy).not.toHaveBeenCalled();
    submitSpy.mockRestore();
  });

  it("does not navigate a closed tab on the URL-template path either", async () => {
    attachTicketMock.mockResolvedValue("tkt_x");
    const tab = openEnterTab()!;
    Object.assign(tabStub, { closed: true });
    await enterUIApp("run-1", "__desktop", { enabled: true, enter_url_template: "http://ui.local/enter?t={ticket}" }, { tab });
    expect(tabStub.location.replace).not.toHaveBeenCalled();
    expect(openSpy).toHaveBeenCalledTimes(1);
  });
});
