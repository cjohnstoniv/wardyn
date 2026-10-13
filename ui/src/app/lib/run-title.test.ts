/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { titleFromTask } from "./run-title";

// boundary within 80 characters, never mid-word.
describe("titleFromTask", () => {
  it("uses the task's first line verbatim when it fits", () => {
    expect(titleFromTask("Refactor the payments module")).toBe("Refactor the payments module");
  });

  it("takes only the FIRST line of a multi-line task", () => {
    expect(titleFromTask("Refactor the payments module\nSee ticket 4412 for details")).toBe(
      "Refactor the payments module",
    );
  });

  it("cuts a long first line at a WORD BOUNDARY within 80 characters, never mid-word", () => {
    const task =
      "Refactor the payments module so a retry never double-charges a customer who already paid";
    const title = titleFromTask(task);
    expect(title.length).toBeLessThanOrEqual(80);
    // Every character up to the cut is still the task's own text — proves the
    // cut never lands mid-word (task[title.length] is either the end of the
    // string or a space).
    expect(task.startsWith(title)).toBe(true);
    expect([" ", undefined]).toContain(task[title.length]);
  });

  it("cuts a single unbroken 80+ character word with no space to fall back to", () => {
    const task = "a".repeat(90);
    expect(titleFromTask(task)).toBe("a".repeat(80));
  });

  it("trims leading/trailing whitespace off the first line", () => {
    expect(titleFromTask("   Refund flow   \nmore")).toBe("Refund flow");
  });

  it("is empty for an empty task, same as an untouched title", () => {
    expect(titleFromTask("")).toBe("");
  });

  // F4 (#1197 L2 review): the task field tolerates tab/CR (the multiline
  // exemption), but a title does not — an interior control character
  // surviving into the prefill got the operator refused on Launch for a
  // title they never typed.
  it("collapses an interior tab into a space", () => {
    expect(titleFromTask("Fix\tthe build")).toBe("Fix the build");
  });

  it("collapses a run of control characters (NUL, tab) into one space", () => {
    expect(titleFromTask("Fix\tthe\u0000build")).toBe("Fix the build");
  });

  it("collapses an interior carriage return the same way", () => {
    expect(titleFromTask("Fix\rthe build")).toBe("Fix the build");
  });
});

// review round 3, R3-1/R3-5: this is the ONE place the wire's own
// omitzero/absent distinction (setup.go) is turned into this module's own
