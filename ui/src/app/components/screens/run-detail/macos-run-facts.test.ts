/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /runs/{id}'s macos_disk onto the Desktop block's disk line (mock M-D1 frames 9 to 11).
import { describe, it, expect } from "vitest";
import { macOSDiskPending, macOSRunFactsOf } from "./macos-run-facts";
import { shortTime } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import { DESKTOP } from "../../wardyn/copy/desktop";

describe("macOSRunFactsOf: the disk", () => {
  it("removing is the removing line", () => {
    expect(macOSRunFactsOf({ macos_disk: { state: "removing" } })).toEqual({ disk: { state: "removing" } });
  });

  it("removed carries its time as the local time of day", () => {
    const at = aheadByHours(-1);
    expect(macOSRunFactsOf({ macos_disk: { state: "removed", at } })).toEqual({ disk: { state: "removed", at: shortTime(at) } });
  });

  it.each<[code: string, reason: string]>([
    ["helper_alive", DESKTOP.diskReasonInUse],
    ["remove_failed", DESKTOP.diskReasonDeleteFailed],
    ["", DESKTOP.diskReasonUnknown],
    ["toString", DESKTOP.diskReasonUnknown],
    ["constructor", DESKTOP.diskReasonUnknown],
    ["future_code", DESKTOP.diskReasonUnknown],
  ])("kept with %j names its reason", (code, reason) => {
    expect(macOSRunFactsOf({ macos_disk: { state: "kept", reason: code } })).toEqual({ disk: { state: "failed", reason } });
  });

  it("a view-only desktop and a disk line are both kept", () => {
    expect(macOSRunFactsOf({ macos: { desktop: "observe" }, macos_disk: { state: "removing" } })).toEqual({
      desktop: "observe",
      disk: { state: "removing" },
    });
  });

  it("neither gives no facts", () => {
    expect(macOSRunFactsOf({})).toBeUndefined();
  });

  it("only a disk still being removed keeps the page polling", () => {
    expect(macOSDiskPending({ macos_disk: { state: "removing" } })).toBe(true);
    expect(macOSDiskPending({ macos_disk: { state: "removed", at: aheadByHours(-1) } })).toBe(false);
    expect(macOSDiskPending({ macos_disk: { state: "kept", reason: "helper_alive" } })).toBe(false);
    expect(macOSDiskPending({})).toBe(false);
    expect(macOSDiskPending(null)).toBe(false);
  });
});
