/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A support helper's doors on the Desktop and Attach tiles (board M5c, ruling 1): the one exception to "user view",
// admitted only for the helper of an OPEN grant on this run, and only when the grant opens a lane of the tile.
import { describe, it, expect } from "vitest";
import { RUN_WIDGETS, type WidgetContext } from "./widget-registry";
import type { RunDetail } from "../../../lib/types";
import type { SupportDoor } from "../../../lib/api/support";

const OWNER = "maya@example.com";
const HELPER = "sam@example.com";
const vm = (over: Partial<RunDetail> = {}): RunDetail =>
  ({ id: "33333333-3333-3333-3333-333333333333", created_by: OWNER, state: "RUNNING", machine_kind: "linux_vm", ...over }) as RunDetail;
type Who = "owner" | "admin, no grant" | "admin, open grant" | "admin, ended grant" | "member";
// What useRunSupport hands the context: the open grant's doors for a helper, null for everyone else (an ended grant included).
const ctx = (who: Who, view: WidgetContext["view"], doors: readonly SupportDoor[] = [], r: RunDetail = vm()): WidgetContext =>
  ({
    run: r,
    view,
    principal: who === "owner" ? OWNER : HELPER,
    operator: who.startsWith("admin"),
    operatorResolved: true,
    helperDoors: who === "admin, open grant" ? doors : null,
  }) as WidgetContext;
const desktop = (c: WidgetContext) => RUN_WIDGETS.desktop.available?.(c);
const attach = (c: WidgetContext) => RUN_WIDGETS.ssh.available?.(c);
const ALL: SupportDoor[] = ["terminal", "commands", "files", "forward", "web_apps", "desktop"];

describe("the Desktop and Attach tiles: who is admitted", () => {
  it.each<[who: Who, view: WidgetContext["view"], desk: boolean, att: boolean]>([
    ["owner", "user", true, true],
    ["owner", "admin", false, false],
    ["admin, no grant", "admin", false, false],
    ["admin, no grant", "user", false, false],
    ["admin, ended grant", "admin", false, false],
    ["member", "user", false, false],
    ["admin, open grant", "admin", true, true],
  ])("%s in the %s view: Desktop %s, Attach %s", (who, view, desk, att) => {
    const c = ctx(who, view, ALL);
    expect(desktop(c)).toBe(desk);
    expect(attach(c)).toBe(att);
  });

  it.each<[doors: SupportDoor[], desk: boolean, att: boolean]>([
    [["desktop"], true, false],
    [["files"], true, true],
    [["terminal"], false, true],
    [["commands"], false, true],
    [["forward"], false, true],
    [["web_apps"], false, true],
    [["desktop", "web_apps"], true, true],
    [[], false, false],
  ])("a helper whose grant opens %j: Desktop %s, Attach %s (a tile with no open lane is not drawn)", (doors, desk, att) => {
    const c = ctx("admin, open grant", "admin", doors);
    expect(desktop(c)).toBe(desk);
    expect(attach(c)).toBe(att);
  });

  it("files alone draws the Desktop tile only where the card draws the Files lane: not a macOS VM with no declared UI app", () => {
    const mac = (over: Partial<RunDetail> = {}) => vm({ machine_kind: "macos_vm", ...over });
    const app = [{ name: "code-server", port: 8080 }];
    expect(desktop(ctx("admin, open grant", "admin", ["files"], mac()))).toBe(false);
    expect(desktop(ctx("admin, open grant", "admin", ["files"], mac({ ui_apps: app })))).toBe(true);
    expect(desktop(ctx("admin, open grant", "admin", ["desktop"], mac()))).toBe(true);
    expect(desktop(ctx("admin, open grant", "admin", ["files"], vm()))).toBe(true);
  });

  it("a helper's Desktop tile needs a live VM run; the Attach tile a live run that is not a background one", () => {
    expect(desktop(ctx("admin, open grant", "admin", ALL, vm({ machine_kind: undefined })))).toBe(false);
    expect(desktop(ctx("admin, open grant", "admin", ALL, vm({ state: "COMPLETED" })))).toBe(false);
    expect(desktop(ctx("admin, open grant", "admin", ALL, vm({ experience: "background" })))).toBe(false);
    expect(attach(ctx("admin, open grant", "admin", ALL, vm({ machine_kind: undefined })))).toBe(true);
    expect(attach(ctx("admin, open grant", "admin", ALL, vm({ state: "COMPLETED" })))).toBe(false);
    expect(attach(ctx("admin, open grant", "admin", ALL, vm({ experience: "background" })))).toBe(false);
  });
});
