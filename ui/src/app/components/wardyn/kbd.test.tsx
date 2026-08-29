/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The whole point of Kbd is the one thing a caller cannot do inline: spell the
// modifier for the platform, and join it the way that platform joins it. A
// Windows operator told to press ⌘ has been told to press a key their keyboard
// does not have.
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { Kbd, MOD, chordLabel } from "./kbd";

/** jsdom's navigator.platform is read-only; redefine it for the test. */
function platform(value: string) {
  Object.defineProperty(navigator, "platform", { value, configurable: true });
}

afterEach(() => {
  cleanup();
  platform("");
});

describe("chordLabel", () => {
  it("spells the modifier per platform", () => {
    expect(chordLabel([MOD, "\\"], true)).toBe("⌘\\");
    expect(chordLabel([MOD, "\\"], false)).toBe("Ctrl+\\");
  });

  it("joins with + only off Apple platforms — ⌘ glyphs abut", () => {
    expect(chordLabel([MOD, "Shift", "F"], true)).toBe("⌘ShiftF");
    expect(chordLabel([MOD, "Shift", "F"], false)).toBe("Ctrl+Shift+F");
  });

  it("leaves a single named key alone on both", () => {
    expect(chordLabel(["Esc"], true)).toBe("Esc");
    expect(chordLabel(["Esc"], false)).toBe("Esc");
  });
});

describe("Kbd", () => {
  it("renders ⌘ on a Mac", () => {
    platform("MacIntel");
    render(<Kbd keys={[MOD, "\\"]} />);
    expect(screen.getByText("⌘\\").tagName).toBe("KBD");
  });

  it("renders Ctrl+ everywhere else", () => {
    platform("Win32");
    render(<Kbd keys={[MOD, "\\"]} />);
    expect(screen.getByText("Ctrl+\\")).toBeInTheDocument();
  });

  it("is a key cap, not a control — nothing to press, nothing to focus", () => {
    platform("Linux x86_64");
    render(<Kbd keys={["Esc"]} />);
    const cap = screen.getByText("Esc");
    expect(cap.tagName).toBe("KBD");
    expect(cap.closest("button")).toBeNull();
  });
});
