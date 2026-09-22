/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { decideKey } from "./attach-terminal-keys";

// #133 — DE/FR/ES layouts type `]` via AltGr, which the browser reports as
// ctrlKey && altKey. A chord bound to bare Ctrl+] is then untypeable: every
// AltGr+9 (DE) / AltGr+) etc. also LOOKS like the escape chord and steals the
// bracket. The table below pins the fix per layout "shape" (how a given
// physical key event arrives, not real IME behavior).

const base = {
  key: "",
  code: "",
  ctrlKey: false,
  altKey: false,
  shiftKey: false,
  metaKey: false,
};

describe("decideKey", () => {
  it.each([
    ["US", { code: "Backspace" }],
    ["DE", { code: "Backspace" }],
    ["FR", { code: "Backspace" }],
    ["ES", { code: "Backspace" }],
  ])("Ctrl+Shift+Backspace escapes on the %s layout shape", (_layout, extra) => {
    const e = { ...base, ...extra, key: "Backspace", ctrlKey: true, shiftKey: true };
    expect(decideKey(e)).toBe("escape");
  });

  it("Ctrl+] with altKey false (US, no AltGr) escapes silently", () => {
    const e = { ...base, key: "]", code: "BracketRight", ctrlKey: true, altKey: false };
    expect(decideKey(e)).toBe("escape");
  });

  it("Ctrl+] with altKey true (DE AltGr+9 typing a bracket) reaches the PTY", () => {
    const e = { ...base, key: "]", code: "Digit9", ctrlKey: true, altKey: true };
    expect(decideKey(e)).toBe("pty");
  });

  it("bare ']' reaches the PTY", () => {
    const e = { ...base, key: "]", code: "BracketRight" };
    expect(decideKey(e)).toBe("pty");
  });

  it("Ctrl+Shift+Enter inserts a newline (unchanged)", () => {
    const e = { ...base, key: "Enter", code: "Enter", ctrlKey: true, shiftKey: true };
    expect(decideKey(e)).toBe("newline");
  });

  it("Ctrl+V pastes (unchanged)", () => {
    const e = { ...base, key: "v", code: "KeyV", ctrlKey: true };
    expect(decideKey(e)).toBe("paste");
  });

  it("plain Backspace reaches the PTY", () => {
    const e = { ...base, key: "Backspace", code: "Backspace" };
    expect(decideKey(e)).toBe("pty");
  });

  it("Ctrl+Backspace (no shift) reaches the PTY", () => {
    const e = { ...base, key: "Backspace", code: "Backspace", ctrlKey: true };
    expect(decideKey(e)).toBe("pty");
  });

  it("Cmd+V on macOS still pastes (metaKey preserved)", () => {
    const e = { ...base, key: "v", code: "KeyV", metaKey: true };
    expect(decideKey(e)).toBe("paste");
  });

  it("Ctrl+] with metaKey held does not escape", () => {
    const e = { ...base, key: "]", code: "BracketRight", ctrlKey: true, metaKey: true };
    expect(decideKey(e)).toBe("pty");
  });
});
