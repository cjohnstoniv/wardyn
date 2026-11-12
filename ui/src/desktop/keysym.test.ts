/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { TERMINAL } from "../app/components/wardyn/copy/terminal";
import { isEscapeChord } from "./keysym";

describe("isEscapeChord", () => {
  const key = { key: "Backspace", ctrlKey: true, shiftKey: true, altKey: false };

  it("is the terminal's advertised chord", () => {
    expect(TERMINAL.ESCAPE_CHORD).toBe("Ctrl+Shift+Backspace");
    expect(isEscapeChord(key)).toBe(true);
  });

  it("is nothing less: a bare Backspace, Ctrl+Backspace, or AltGr (Ctrl+Alt) with it goes to the desktop", () => {
    expect(isEscapeChord({ ...key, ctrlKey: false })).toBe(false);
    expect(isEscapeChord({ ...key, shiftKey: false })).toBe(false);
    expect(isEscapeChord({ ...key, altKey: true })).toBe(false);
    expect(isEscapeChord({ ...key, key: "a" })).toBe(false);
  });
});
