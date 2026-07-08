/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { isTerminalRunState, TERMINAL_RUN_STATES } from "./types";
import type { RunState } from "./types";

describe("isTerminalRunState", () => {
  // Regression for the COMPLETED-state cluster: COMPLETED is terminal-success
  // and must be treated as terminal (Kill button disabled, no re-poll).
  it("treats COMPLETED as terminal", () => {
    expect(isTerminalRunState("COMPLETED")).toBe(true);
  });

  it.each(["STOPPED", "ARCHIVED", "FAILED", "KILLED"] as RunState[])(
    "treats %s as terminal",
    (s) => expect(isTerminalRunState(s)).toBe(true),
  );

  it.each(["PENDING", "STARTING", "RUNNING", "WAITING_FOR_CONFIRMATION"] as RunState[])(
    "treats %s as non-terminal",
    (s) => expect(isTerminalRunState(s)).toBe(false),
  );

  it("treats an unknown backend state as non-terminal (fail-soft, killable)", () => {
    expect(isTerminalRunState("SOME_FUTURE_STATE" as RunState)).toBe(false);
  });

  it("exposes COMPLETED in the terminal set", () => {
    expect(TERMINAL_RUN_STATES).toContain("COMPLETED");
  });
});
