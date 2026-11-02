/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Attach from your terminal" tile: a finished macOS VM run keeps it while its disk line has something to
// say (mock M-D1 frames 9 to 11), and its place is the one migration 0236_macos_vm_disks writes into saved
// finished layouts.
import { describe, it, expect } from "vitest";
import { RUN_WIDGETS, addWidget, presetLayout, type WidgetContext } from "./widget-registry";
import type { RunDetail } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";

const OWNER = "alice@example.com";
const run = (over: Partial<RunDetail> = {}): RunDetail =>
  ({ id: "22222222-2222-2222-2222-222222222222", created_by: OWNER, state: "RUNNING", machine_kind: "", ...over }) as RunDetail;
const ctx = (r: RunDetail, view: WidgetContext["view"] = "user"): WidgetContext =>
  ({ run: r, view, principal: OWNER, operator: false, operatorResolved: true }) as WidgetContext;
const available = (c: WidgetContext) => RUN_WIDGETS.ssh.available?.(c) ?? true;
const removed = { state: "removed", at: aheadByHours(-1) } as const;

describe("RUN_WIDGETS.ssh", () => {
  it.each<[name: string, c: WidgetContext, want: boolean]>([
    ["a running owner's container run", ctx(run()), true],
    ["that run finished", ctx(run({ state: "COMPLETED" })), false],
    ["a finished linux_vm run with a macos_disk", ctx(run({ state: "COMPLETED", machine_kind: "linux_vm", macos_disk: removed })), false],
    ["a finished macos_vm run without a macos_disk", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm" })), false],
    ["a finished macos_vm run with one", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm", macos_disk: removed })), true],
    ["that run in the admin view", ctx(run({ state: "COMPLETED", machine_kind: "macos_vm", macos_disk: removed }), "admin"), false],
  ])("is available for %s: %s", (_name, c, want) => {
    expect(available(c)).toBe(want);
  });

  it("the finished preset places it at 6,12,6,5", () => {
    expect(presetLayout("finished").find((w) => w.widget === "ssh")).toEqual({ widget: "ssh", x: 6, y: 12, w: 6, h: 5 });
  });

  it("an add puts it where migration 0236 does: x 0, below everything, 4 by 5", () => {
    const layout = [
      { widget: "terminal" as const, x: 0, y: 0, w: 7, h: 8 },
      { widget: "identity" as const, x: 6, y: 8, w: 6, h: 4 },
    ];
    const added = addWidget(layout, "ssh");
    expect(added[added.length - 1]).toEqual({ widget: "ssh", x: 0, y: 12, w: 4, h: 5 });
  });
});
