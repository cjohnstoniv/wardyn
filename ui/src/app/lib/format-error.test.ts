/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { getErrorMessage } from "./format";

describe("getErrorMessage", () => {
  it("returns the message, or the String() of a non-Error", () => {
    expect(getErrorMessage(new Error("boom"))).toBe("boom");
    expect(getErrorMessage("plain")).toBe("plain");
  });

  it("never returns blank text: an empty message gets the fallback", () => {
    expect(getErrorMessage(new Error(""))).toBe("No reason was given.");
    expect(getErrorMessage(new Error("  "), "Failed to add key.")).toBe("Failed to add key.");
  });
});
