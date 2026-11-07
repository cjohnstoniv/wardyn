/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Attach from your terminal" tile (a running run the person may enter, never a background one: it has no terminal)
// and the Desktop tile that took the desktop block out of it. A finished macOS VM run keeps the Desktop tile while
// its disk line has something to say, at the place the Attach tile held it.
import { describe, it, expect } from "vitest";
import { RUN_WIDGETS, WIDGET_IDS, addWidget, presetLayout, type WidgetContext } from "./widget-registry";
import type { RunDetail } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";

const OWNER = "alice@example.com";
const run = (over: Partial<RunDetail> = {}): RunDetail =>
  ({ id: "22222222-2222-2222-2222-222222222222", created_by: OWNER, state: "RUNNING", machine_kind: "", ...over }) as RunDetail;
const ctx = (r: RunDetail, view: WidgetContext["view"] = "user"): WidgetContext =>
  ({ run: r, view, principal: OWNER, operator: false, operatorResolved: true }) as WidgetContext;
const available = (c: WidgetContext) => RUN_WIDGETS.ssh.available?.(c) ?? true;
const desktopAvailable = (c: WidgetContext) => RUN_WIDGETS.desktop.available?.(c) ?? true;
const removed = { state: "removed", at: aheadByHours(-1) } as const;

describe("RUN_WIDGETS.ssh", () => {
  it.each<[name: string, c: WidgetContext, want: boolean]>([
    ["a running owner's container run", ctx(run()), true],
    ["that run finished", ctx(run({ state: "COMPLETED" })), false],
    ["a finished linux_vm run with a macos_disk", ctx(run({ state: "COMPLETED", machine_kind: "linux_vm", macos_disk: removed })), false],
    ["a finished macos_vm run without a macos_disk", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm" })), false],
    ["a finished macos_vm run with one: its disk line is the Desktop tile's", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm", macos_disk: removed })), false],
    ["a running background container run: no terminal to attach to", ctx(run({ experience: "background" })), false],
    ["a running background macos_vm run", ctx(run({ experience: "background", machine_kind: "macos_vm" })), false],
    ["a running interactive run", ctx(run({ experience: "interactive" })), true],
    ["a running run from before the field", ctx(run({ experience: undefined })), true],
    ["a running run in the admin view", ctx(run(), "admin"), false],
  ])("is available for %s: %s", (_name, c, want) => {
    expect(available(c)).toBe(want);
  });

  it("the finished preset has no place for it: a finished run has nothing to attach to", () => {
    expect(presetLayout("finished").find((w) => w.widget === "ssh")).toBeUndefined();
  });

  it("the live preset of a VM run puts it under the Desktop tile, and Effective policy under it", () => {
    const at = (id: string) => presetLayout("live", false, true).find((w) => w.widget === id);
    expect(at("desktop")).toEqual({ widget: "desktop", x: 8, y: 24, w: 4, h: 4 });
    expect(at("ssh")).toEqual({ widget: "ssh", x: 8, y: 28, w: 4, h: 5 });
    expect(at("effective-policy")?.y).toBe(33);
  });

  it("the live preset of any other run has no Desktop tile and no gap: Attach at 24, Effective policy at 29", () => {
    const l = presetLayout("live");
    expect(l.find((w) => w.widget === "desktop")).toBeUndefined();
    expect(l.find((w) => w.widget === "ssh")).toEqual({ widget: "ssh", x: 8, y: 24, w: 4, h: 5 });
    expect(l.find((w) => w.widget === "effective-policy")?.y).toBe(29);
  });

  it("the finished preset of any other run has no Desktop tile", () => {
    expect(presetLayout("finished").find((w) => w.widget === "desktop")).toBeUndefined();
    expect(presetLayout("finished").find((w) => w.widget === "effective-policy")).toEqual({ widget: "effective-policy", x: 0, y: 12, w: 6, h: 4 });
  });

  it("an add puts it below everything, 4 by 5", () => {
    const layout = [
      { widget: "terminal" as const, x: 0, y: 0, w: 7, h: 8 },
      { widget: "identity" as const, x: 6, y: 8, w: 6, h: 4 },
    ];
    const added = addWidget(layout, "ssh");
    expect(added[added.length - 1]).toEqual({ widget: "ssh", x: 0, y: 12, w: 4, h: 5 });
  });
});

describe("RUN_WIDGETS.desktop", () => {
  it.each<[name: string, c: WidgetContext, want: boolean]>([
    ["a running linux_vm run", ctx(run({ machine_kind: "linux_vm" })), true],
    ["a running macos_vm run", ctx(run({ machine_kind: "macos_vm" })), true],
    ["a running background macos_vm run: its Watch lives here", ctx(run({ machine_kind: "macos_vm", experience: "background" })), true],
    ["a running container run: it has no desktop", ctx(run()), false],
    ["a finished linux_vm run", ctx(run({ state: "COMPLETED", machine_kind: "linux_vm", macos_disk: removed })), false],
    ["a finished macos_vm run without a macos_disk", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm" })), false],
    ["a finished macos_vm run with one", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm", macos_disk: removed })), true],
    ["that run in the admin view", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm", macos_disk: removed }), "admin"), false],
    ["a running run another person started", { ...ctx(run({ machine_kind: "linux_vm", created_by: "bob@example.com" })) }, false],
  ])("is available for %s: %s", (_name, c, want) => {
    expect(desktopAvailable(c)).toBe(want);
  });

  it("the finished preset of a VM run places it where the Attach tile held the disk line, 6,12,6,5", () => {
    expect(presetLayout("finished", false, true).find((w) => w.widget === "desktop")).toEqual({ widget: "desktop", x: 6, y: 12, w: 6, h: 5 });
  });

  it("is a registered widget id, labelled as its tile is, so a tile's expand view can read it", () => {
    expect(WIDGET_IDS).toContain("desktop");
    expect(RUN_WIDGETS.desktop.label).toBe("Desktop");
  });
});
