/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// jsdom runs no CSS animation, so Radix's Presence unmounts a closed layer at once. The console's dialogs
// have a 200 ms exit animation, and some close handlers (focus restore) only run once it ends. This makes a
// closed layer report a running exit animation and holds it until `finish`, so a test can place focus in
// between, as a caller does.
import { act } from "@testing-library/react";
import { vi } from "vitest";

export function holdExitAnimation(): { finish: () => Promise<void>; release: () => void } {
  const real = window.getComputedStyle.bind(window);
  // Presence keeps the style object it read when the node mounted and reads it again on close, so the answer
  // follows the node's data-state rather than being fixed when the object is made.
  const spy = vi.spyOn(window, "getComputedStyle").mockImplementation(((el: Element, pseudo?: string | null) => {
    if (!el.hasAttribute?.("data-state")) return real(el, pseudo);
    return new Proxy({} as CSSStyleDeclaration, {
      get: (_, key) => {
        if (key === "animationName") return el.getAttribute("data-state") === "closed" ? "exit" : "none";
        const style = real(el, pseudo);
        const value = Reflect.get(style, key, style);
        return typeof value === "function" ? value.bind(style) : value;
      },
    });
  }) as typeof window.getComputedStyle);
  return {
    finish: async () => {
      await act(async () => {
        for (const el of Array.from(document.querySelectorAll('[data-state="closed"]'))) {
          el.dispatchEvent(Object.assign(new Event("animationend", { bubbles: true }), { animationName: "exit" }));
        }
      });
      // Radix hands focus back one macrotask after the layer unmounts.
      await act(async () => { await new Promise((r) => setTimeout(r, 10)); });
    },
    release: () => spy.mockRestore(),
  };
}
