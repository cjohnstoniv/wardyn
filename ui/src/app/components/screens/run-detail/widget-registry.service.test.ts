/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Service widget's place: the head of the live right column for a service run, and nowhere else.
import { describe, it, expect } from "vitest";
import { RUN_WIDGETS, forRun, presetLayout, type WidgetContext } from "./widget-registry";
import type { RunDetail } from "../../../lib/types";

const at = (layout: ReturnType<typeof presetLayout>, id: string) => layout.find((w) => w.widget === id);
const ctx = (over: Partial<RunDetail>) => ({ run: { state: "RUNNING", ...over } as RunDetail }) as WidgetContext;

describe("the Service widget's layout", () => {
  it("is absent from every default layout but a service run's live one", () => {
    expect(at(presetLayout("live"), "service")).toBeUndefined();
    expect(at(presetLayout("finished"), "service")).toBeUndefined();
    expect(at(presetLayout("finished", true), "service")).toBeUndefined();
  });

  it("heads the right column of a service run's live layout, and moves the column down by its height", () => {
    const plain = presetLayout("live");
    const live = presetLayout("live", true);
    const svc = at(live, "service")!;
    expect(svc).toMatchObject({ x: 8, y: 0 });
    for (const w of plain) {
      const moved = at(live, w.widget)!;
      expect(moved.y).toBe(w.x >= 8 ? w.y + svc.h : w.y);
    }
    expect(at(live, "egress")!.y).toBe(svc.h);
  });

  it("is added to a saved layout that predates it, once", () => {
    const saved = [{ widget: "terminal", x: 0, y: 0, w: 8, h: 12 }, { widget: "egress", x: 8, y: 0, w: 4, h: 4 }];
    const out = forRun(saved, "live", true);
    expect(at(out, "service")).toMatchObject({ x: 8, y: 0 });
    expect(at(out, "egress")!.y).toBeGreaterThan(0);
    expect(forRun(out, "live", true)).toBe(out);
    expect(forRun(saved, "live", false)).toBe(saved);
    expect(forRun(saved, "finished", true)).toBe(saved);
  });

  it("is available on a RUNNING service run only", () => {
    const avail = (c: WidgetContext) => RUN_WIDGETS.service.available?.(c);
    expect(avail(ctx({ service: { restart: "never" } }))).toBe(true);
    expect(avail(ctx({}))).toBe(false);
    expect(avail(ctx({ service: { restart: "never" }, state: "COMPLETED" }))).toBe(false);
  });
});
