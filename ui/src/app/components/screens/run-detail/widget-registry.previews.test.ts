/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Preview URLs tile's place and its gate: below the rail's others in the live preset, added to a saved live layout that
// predates it, kept out of one saved with it removed; drawn for the run's person or one who may enter it and for a security
// admin, while the site switch is on and the run has not ended.
import { describe, it, expect } from "vitest";
import {
  PREVIEWS_REMOVED,
  RUN_WIDGETS,
  WIDGET_IDS,
  forPreviews,
  isPreviewsRemoved,
  normalizeLayout,
  presetLayout,
  type WidgetContext,
} from "./widget-registry";
import type { RunDetail } from "../../../lib/types";

const at = (layout: ReturnType<typeof presetLayout>, id: string) => layout.find((w) => w.widget === id);
const OWNER = "alice@example.com";

function ctx(over: Partial<Omit<WidgetContext, "run">> & { run?: Record<string, unknown> } = {}): WidgetContext {
  const { run, ...rest } = over;
  return {
    run: { state: "RUNNING", created_by: OWNER, ...run } as RunDetail,
    finished: false,
    principal: OWNER,
    operator: false,
    operatorResolved: true,
    view: "user",
    previewsOn: true,
    ...rest,
  } as WidgetContext;
}
const available = (c: WidgetContext) => RUN_WIDGETS.previews.available?.(c);

describe("the Preview URLs widget's layout", () => {
  it("is a widget id the server accepts, labelled Preview URLs", () => {
    expect(WIDGET_IDS).toContain("previews");
    expect(RUN_WIDGETS.previews.label).toBe("Preview URLs");
  });

  it("is in the live preset at the foot of the right rail, below Attach and Effective policy, overlapping nothing", () => {
    for (const vm of [false, true]) {
      const layout = presetLayout("live", false, vm);
      const tile = at(layout, "previews")!;
      expect(tile.x).toBe(8);
      for (const w of layout.filter((o) => o !== tile)) {
        const overlap = w.x < tile.x + tile.w && tile.x < w.x + w.w && w.y < tile.y + tile.h && tile.y < w.y + w.h;
        expect(overlap, `${w.widget} vm=${vm}`).toBe(false);
      }
      expect(tile.y).toBeGreaterThanOrEqual(at(layout, "ssh")!.y + at(layout, "ssh")!.h);
    }
    expect(at(presetLayout("live"), "previews")).toEqual({ widget: "previews", x: 8, y: 33, w: 4, h: 5 });
    expect(at(presetLayout("live", false, true), "previews")).toEqual({ widget: "previews", x: 8, y: 37, w: 4, h: 5 });
  });

  it("has no slot in the finished preset", () => {
    expect(at(presetLayout("finished"), "previews")).toBeUndefined();
    expect(at(presetLayout("finished", false, true, true), "previews")).toBeUndefined();
  });

  it("is added to a saved live layout that predates it, once, at the foot of the right column", () => {
    const saved = [{ widget: "terminal", x: 0, y: 0, w: 8, h: 12 }, { widget: "egress", x: 8, y: 0, w: 4, h: 4 }];
    const out = forPreviews(saved, "live");
    expect(at(out, "previews")).toEqual({ widget: "previews", x: 8, y: 4, w: 4, h: 5 });
    expect(forPreviews(out, "live")).toBe(out);
  });

  it("leaves a saved finished layout alone", () => {
    const saved = [{ widget: "terminal", x: 0, y: 0, w: 8, h: 12 }];
    expect(forPreviews(saved, "finished")).toBe(saved);
  });

  it("drops the removal marker from a normalized layout, and knows it only at 1x1", () => {
    expect(normalizeLayout([{ widget: "terminal", x: 0, y: 0, w: 8, h: 12 }, PREVIEWS_REMOVED], "live").map((w) => w.widget)).toEqual(["terminal"]);
    expect(isPreviewsRemoved(PREVIEWS_REMOVED)).toBe(true);
    expect(isPreviewsRemoved({ widget: "previews", x: 8, y: 33, w: 4, h: 5 })).toBe(false);
  });
});

describe("the Preview URLs tile's gate", () => {
  it("is drawn for the run's person while the switch is on and the run has not ended", () => {
    expect(available(ctx())).toBe(true);
  });

  it("is not drawn with the switch off, or before the setup answer has said", () => {
    expect(available(ctx({ previewsOn: false }))).toBe(false);
    expect(available(ctx({ previewsOn: undefined }))).toBe(false);
  });

  it("is not drawn for a run that has ended", () => {
    expect(available(ctx({ finished: true }))).toBe(false);
  });

  it("is drawn on a background run: the tile says why it has no previews", () => {
    expect(available(ctx({ run: { experience: "background" } }))).toBe(true);
  });

  it("is drawn for a security admin on a person's run, and not for anyone else", () => {
    expect(available(ctx({ principal: "carol@example.com" }))).toBe(false);
    expect(available(ctx({ principal: "carol@example.com", securityOperator: false }))).toBe(false);
    expect(available(ctx({ principal: "carol@example.com", securityOperator: true }))).toBe(true);
  });

  it("is not drawn for an admin who is not a security admin on a run no person owns", () => {
    const owned = { created_by: "", operator_owned: true };
    expect(available(ctx({ principal: "root@example.com", operator: true, securityOperator: false, run: owned }))).toBe(false);
    expect(available(ctx({ principal: "root@example.com", operator: true, securityOperator: true, run: owned }))).toBe(true);
  });

  it("is not drawn for a service identity's sponsor, and is for a security admin on its run", () => {
    const service = { created_by: "service:ci-bot", operator_owned: false };
    expect(available(ctx({ principal: "sam@example.com", securityOperator: false, run: service }))).toBe(false);
    expect(available(ctx({ principal: "service:ci-bot", securityOperator: false, run: service }))).toBe(false);
    expect(available(ctx({ principal: "sam@example.com", securityOperator: true, run: service }))).toBe(true);
  });

  it("is drawn for the admin token on a run it created only by the security admin arm", () => {
    const own = { created_by: "admin-token", operator_owned: true };
    expect(available(ctx({ principal: "admin-token", operator: true, securityOperator: true, run: own }))).toBe(true);
    expect(available(ctx({ principal: "admin-token", operator: true, securityOperator: false, run: own }))).toBe(false);
  });

  it("is not drawn while who is looking is not known, for the run's person or a security admin", () => {
    expect(available(ctx({ operatorResolved: false }))).toBe(false);
    expect(available(ctx({ principal: "carol@example.com", securityOperator: true, operatorResolved: false }))).toBe(false);
  });

  it("is drawn in the admin view too: the gate does not read the view", () => {
    expect(available(ctx({ view: "admin", securityOperator: true, principal: "carol@example.com" }))).toBe(true);
    expect(available(ctx({ view: "admin" }))).toBe(true);
  });
});
