/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The window on the Mac's draft rules: the switch reads off unless explicitly on, a choice is the at_start key or no
// key, a refusal clears it only by its field, and leaving macOS takes it away.
import { describe, expect, it } from "vitest";
import { chooseMachineKind } from "../../lenses/run-lens";
import type { RunIntent } from "../../draft/run-intent";
import { chooseWindow, macWindowOn, windowRefusal } from "./window-choice";

const MAC: RunIntent = { machine_kind: "macos_vm", desktop_window: "at_start" };

describe("window choice", () => {
  it("reads the site switch from the setup status: only an explicit true is on", () => {
    expect(macWindowOn(null)).toBe(false);
    expect(macWindowOn({})).toBe(false);
    expect(macWindowOn({ features: {} })).toBe(false);
    expect(macWindowOn({ features: { mac_window: false } })).toBe(false);
    expect(macWindowOn({ features: { mac_window: true } })).toBe(true);
  });

  it("writes at_start when ticked and the absent key when not", () => {
    expect(chooseWindow({ machine_kind: "macos_vm" }, true).desktop_window).toBe("at_start");
    expect("desktop_window" in chooseWindow(MAC, false)).toBe(false);
  });

  it("is refused only by a refusal naming desktop_window, while the draft still asks for one", () => {
    expect(windowRefusal(MAC, { field: "desktop_window", text: "off" })).toBe("off");
    expect(windowRefusal(MAC, { field: "", text: "off" })).toBeUndefined();
    expect(windowRefusal(MAC, { field: "machine_kind", text: "off" })).toBeUndefined();
    expect(windowRefusal(MAC, { field: "desktop_window", text: null })).toBeUndefined();
    expect(windowRefusal(MAC, null)).toBeUndefined();
    expect(windowRefusal({ machine_kind: "macos_vm" }, { field: "desktop_window", text: "off" })).toBeUndefined();
  });

  it("goes with macOS: a Linux VM or a container drops it, staying on macOS keeps it", () => {
    expect(chooseMachineKind(MAC, "linux_vm").desktop_window).toBeUndefined();
    expect(chooseMachineKind(MAC, "container").desktop_window).toBeUndefined();
    expect(chooseMachineKind(MAC, "macos_vm").desktop_window).toBe("at_start");
  });
});
