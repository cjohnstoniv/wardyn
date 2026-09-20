/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Finding 7a: openAuthTab is the whole fix — a tab opened ON THE CLICK
// (before any await), with its `opener` severed by hand so the handle
// survives to be navigated later. Covered directly, without the pane, so a
// refactor that reintroduces `window.open(url, …)` from inside a PTY callback
// fails here first.
import { describe, it, expect, vi, beforeEach, type MockInstance } from "vitest";
import { openAuthTab, AUTH_TAB_PLACEHOLDER_HTML } from "./auth-tab-handle";

describe("openAuthTab", () => {
  let fakeWindow: { opener: unknown; location: { href: string }; closed: boolean; close: () => void; document: { write: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn> } };
  let openSpy: MockInstance<typeof window.open>;

  beforeEach(() => {
    fakeWindow = {
      opener: {},
      location: { href: "about:blank" },
      closed: false,
      close: vi.fn(function (this: typeof fakeWindow) {
        this.closed = true;
      }),
      document: { write: vi.fn(), close: vi.fn() },
    };
    openSpy = vi.spyOn(window, "open").mockReturnValue(fakeWindow as unknown as Window);
  });

  it("opens empty, with no noopener feature string", () => {
    openAuthTab();
    expect(openSpy).toHaveBeenCalledWith("", "_blank");
  });

  it("writes the provider-neutral placeholder document", () => {
    openAuthTab();
    expect(fakeWindow.document.write).toHaveBeenCalledWith(AUTH_TAB_PLACEHOLDER_HTML);
    expect(fakeWindow.document.close).toHaveBeenCalled();
  });

  // The mitigation IS severing opener — order matters: it must happen before
  // any navigation could occur, so recorded here as "set before navigate is
  // ever called" (navigate is a later, separate call in this lane).
  it("severs opener before the handle is ever navigated", () => {
    const tab = openAuthTab();
    expect(fakeWindow.opener).toBeNull();
    tab?.navigate("https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD");
    expect(fakeWindow.location.href).toBe("https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD");
  });

  it("a blocked popup yields null, and every later call is a no-op", () => {
    openSpy.mockReturnValue(null);
    const tab = openAuthTab();
    expect(tab).toBeNull();
  });

  it("close() closes the real window handle", () => {
    const tab = openAuthTab();
    tab?.close();
    expect(fakeWindow.close).toHaveBeenCalled();
  });

  it("close() is a harmless no-op once the tab is already closed", () => {
    fakeWindow.closed = true;
    const tab = openAuthTab();
    tab?.close();
    expect(fakeWindow.close).not.toHaveBeenCalled();
  });
});
