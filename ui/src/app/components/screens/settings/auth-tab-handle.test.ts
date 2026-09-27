/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Finding 7a: openSignInTab opens blank, severs `opener` by hand, then
// navigates — covered directly, without the pane, so a refactor that drops the
// severing or the blocked-popup signal fails here first.
import { describe, it, expect, vi, beforeEach, type MockInstance } from "vitest";
import { openSignInTab } from "./auth-tab-handle";

const URL = "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH";

describe("openSignInTab", () => {
  let fakeWindow: { opener: unknown; location: { href: string } };
  let openSpy: MockInstance<typeof window.open>;
  let openerAtNavigate: unknown;
  let navigatedTo: string;

  beforeEach(() => {
    openerAtNavigate = "never navigated";
    navigatedTo = "";
    const location = {} as { href: string };
    fakeWindow = { opener: {}, location };
    Object.defineProperty(location, "href", {
      configurable: true,
      get: () => navigatedTo,
      set: (v: string) => {
        openerAtNavigate = fakeWindow.opener;
        navigatedTo = v;
      },
    });
    openSpy = vi.spyOn(window, "open").mockClear().mockReturnValue(fakeWindow as unknown as Window);
  });

  it("opens empty, with no noopener feature string, then navigates to the provider's page", () => {
    expect(openSignInTab(URL)).toBe(true);
    expect(openSpy).toHaveBeenCalledWith("", "_blank");
    expect(navigatedTo).toBe(URL);
  });

  it("severs opener before it navigates", () => {
    openSignInTab(URL);
    expect(fakeWindow.opener).toBeNull();
    expect(openerAtNavigate).toBeNull();
  });

  it("a blocked popup answers false", () => {
    openSpy.mockReturnValue(null);
    expect(openSignInTab(URL)).toBe(false);
  });
});
