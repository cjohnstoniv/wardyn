/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render } from "@testing-library/react";
import { ThemeProvider, useTheme } from "./theme-provider";

function Probe() {
  const { theme } = useTheme();
  return <span>{theme}</span>;
}

// U119: ThemeProvider sits ABOVE the app's only ErrorBoundary (App.tsx wraps
// <Routes> in ThemeProvider; AppShell's ErrorBoundary is a descendant), so a
// throw here is uncatchable and white-screens the whole app. Private-mode Safari
// (and any storage-blocked browser) throws on localStorage access.
describe("ThemeProvider (U119)", () => {
  it("does not crash when localStorage throws (private-mode browsers)", () => {
    const getItemSpy = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("access denied");
    });
    const setItemSpy = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("access denied");
    });
    try {
      expect(() =>
        render(
          <ThemeProvider>
            <Probe />
          </ThemeProvider>,
        ),
      ).not.toThrow();
    } finally {
      getItemSpy.mockRestore();
      setItemSpy.mockRestore();
    }
  });
});
