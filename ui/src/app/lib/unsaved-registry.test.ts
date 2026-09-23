/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { renderHook } from "@testing-library/react";
import { registerUnsaved, unsavedSnapshot, useRegisterUnsaved } from "./unsaved-registry";

describe("unsaved-registry", () => {
  it("unsavedSnapshot is null when nothing is registered", () => {
    expect(unsavedSnapshot()).toBeNull();
  });

  it("registers, unregisters via the returned function, and joins multiple entries with a blank line", () => {
    const unregisterA = registerUnsaved("a", () => "draft A");
    expect(unsavedSnapshot()).toBe("draft A");

    const unregisterB = registerUnsaved("b", () => "draft B");
    expect(unsavedSnapshot()).toBe("draft A\n\ndraft B");

    unregisterA();
    expect(unsavedSnapshot()).toBe("draft B");

    unregisterB();
    expect(unsavedSnapshot()).toBeNull();
  });

  it("a second registerUnsaved with the same id overwrites the first, not appends", () => {
    registerUnsaved("dup", () => "first");
    const unregister = registerUnsaved("dup", () => "second");
    expect(unsavedSnapshot()).toBe("second");
    unregister();
  });

  it("reads getText lazily, at snapshot time — not at registration time", () => {
    let value = "before";
    const unregister = registerUnsaved("lazy", () => value);
    value = "after";
    expect(unsavedSnapshot()).toBe("after");
    unregister();
  });

  describe("useRegisterUnsaved", () => {
    it("registers only while dirty, and unregisters when dirty flips false", () => {
      const { rerender, unmount } = renderHook(({ dirty }) => useRegisterUnsaved("hook-id", dirty, () => "hook text"), {
        initialProps: { dirty: false },
      });
      expect(unsavedSnapshot()).toBeNull();

      rerender({ dirty: true });
      expect(unsavedSnapshot()).toBe("hook text");

      rerender({ dirty: false });
      expect(unsavedSnapshot()).toBeNull();

      unmount();
      expect(unsavedSnapshot()).toBeNull();
    });

    it("unregisters on unmount while still dirty", () => {
      const { unmount } = renderHook(() => useRegisterUnsaved("unmount-id", true, () => "still dirty"));
      expect(unsavedSnapshot()).toBe("still dirty");
      unmount();
      expect(unsavedSnapshot()).toBeNull();
    });

    it("a fresh getText closure every render still reads live — no re-register needed to stay current", () => {
      let calls = 0;
      const { rerender, unmount } = renderHook(
        ({ dirty }) => {
          calls += 1;
          // A fresh closure every render — the common case (it closes over
          // the latest draft) — must not need a re-registration to read
          // live: the hook reads it through a ref, not an effect dependency.
          useRegisterUnsaved("stable-id", dirty, () => `render ${calls}`);
        },
        { initialProps: { dirty: true } },
      );
      expect(unsavedSnapshot()).toBe("render 1");
      rerender({ dirty: true });
      expect(unsavedSnapshot()).toBe("render 2");
      unmount();
    });
  });
});
