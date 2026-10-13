/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { includedMark } from "./catalogue-pending";

describe("includedMark", () => {
  it("marks each harness as the prototype does, and another harness by its own name rather than Claude's mark", () => {
    expect(includedMark({ from: "harness", name: "claude-code" })).toBe("cc");
    expect(includedMark({ from: "harness", name: "codex-cli" })).toBe("cx");
    expect(includedMark({ from: "harness", name: "gemini-cli" })).toBe("ge");
  });

  it("marks the other sources", () => {
    expect(includedMark({ from: "repository", name: "acme/web" })).toBe("git");
    expect(includedMark({ from: "drive", name: "Your drive" })).toBe("dr");
    expect(includedMark({ from: "secret", name: "TOKEN" })).toBe("$");
  });
});
