/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Machine widget's place (board KM-3): the foot of the right column for a run that keeps or resumed a kept machine,
// and nowhere else; a saved layout that predates it gets it; one saved with it taken out does not.
import { describe, it, expect } from "vitest";
import { MACHINE_REMOVED, RUN_WIDGETS, WIDGET_IDS, forMachineRun, normalizeLayout, presetLayout, type WidgetContext } from "./widget-registry";
import type { RunDetail } from "../../../lib/types";

const at = (layout: ReturnType<typeof presetLayout>, id: string) => layout.find((w) => w.widget === id);
const ctx = (over: Partial<RunDetail>) => ({ run: { state: "RUNNING", ...over } as RunDetail }) as WidgetContext;

describe("the Machine widget's layout", () => {
  it("is a widget id the server accepts, labelled Machine", () => {
    expect(WIDGET_IDS).toContain("machine");
    expect(RUN_WIDGETS.machine.label).toBe("Machine");
  });

  it("is absent from every default layout but a machine run's", () => {
    expect(at(presetLayout("live"), "machine")).toBeUndefined();
    expect(at(presetLayout("finished", false, true), "machine")).toBeUndefined();
    expect(at(presetLayout("live", false, true, true), "machine")).toBeDefined();
    expect(at(presetLayout("finished", false, true, true), "machine")).toBeDefined();
  });

  it("sits at the foot of the right column, under what is there, overlapping nothing", () => {
    for (const preset of ["live", "finished"] as const) {
      const layout = presetLayout(preset, false, true, true);
      const tile = at(layout, "machine")!;
      for (const w of layout.filter((o) => o !== tile)) {
        const overlap = w.x < tile.x + tile.w && tile.x < w.x + w.w && w.y < tile.y + tile.h && tile.y < w.y + w.h;
        expect(overlap, `${w.widget} in ${preset}`).toBe(false);
      }
      expect(tile.y).toBeGreaterThan(0);
    }
  });

  it("is added to a saved layout that predates it, once, and leaves other runs' layouts alone", () => {
    const saved = [{ widget: "terminal", x: 0, y: 0, w: 8, h: 12 }, { widget: "egress", x: 8, y: 0, w: 4, h: 4 }];
    const out = forMachineRun(saved, true);
    expect(at(out, "machine")).toMatchObject({ x: 8, y: 4 });
    expect(forMachineRun(out, true)).toBe(out);
    expect(forMachineRun(saved, false)).toBe(saved);
  });

  it("drops the removal marker from a normalized layout", () => {
    expect(normalizeLayout([{ widget: "terminal", x: 0, y: 0, w: 8, h: 12 }, MACHINE_REMOVED], "live").map((w) => w.widget)).toEqual(["terminal"]);
  });

  it("has a tile only for a run that has a machine", () => {
    expect(RUN_WIDGETS.machine.available?.(ctx({}))).toBe(false);
    expect(RUN_WIDGETS.machine.available?.(ctx({ machine: { id: "m", scrubbed: true, deleted: false } }))).toBe(true);
  });
});
