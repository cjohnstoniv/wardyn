/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The canvas's contract with the layout API, pinned where it is cheap to pin:
// a saved arrangement beats the situational default, "never saved" is a 200
// and not a failure, a save sends the wire shape the server validates, a
// deployment that cannot persist degrades to this session instead of erroring,
// and the terminal survives every route by which a layout could lose it.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const getLayout = vi.fn();
const putLayout = vi.fn();
vi.mock("../../../lib/api/run-layout", () => ({
  runLayout: {
    getLayout: (...a: unknown[]) => getLayout(...a),
    putLayout: (...a: unknown[]) => putLayout(...a),
  },
}));
// The two polling evidence widgets fetch on mount; this suite is about the
// canvas, not them (widgets.test.tsx owns those).
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
  },
}));

import { HttpError } from "../../../lib/api/core";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { RunCanvas } from "./canvas";
import { GRID_COLS, GRID_ROWS, presetLayout, type WidgetContext } from "./widget-registry";

const RUN = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

function ctx(overrides: Partial<WidgetContext> = {}): WidgetContext {
  return {
    run: RUN as unknown as WidgetContext["run"],
    finished: false,
    // null keeps the ssh widget unavailable (it is owner-only), so this suite
    // never has to stand up the health / ssh-key fetches.
    principal: null,
    grants: [],
    egress: [],
    audit: [],
    onGoAudit: () => {},
    terminalPane: <div>the session</div>,
    ...overrides,
  };
}

/** The y of a grid item's CSS transform, for "is this tile above that one". */
function tileY(el: Element | null | undefined): number {
  const item = el?.closest(".react-grid-item") as HTMLElement | null;
  const m = /translate\((-?[\d.]+)px,\s*(-?[\d.]+)px\)/.exec(item?.style.transform ?? "");
  return m ? Number(m[2]) : Number.NaN;
}

const widgetHeading = (name: string) => screen.queryByRole("heading", { name });

beforeEach(() => {
  getLayout.mockReset();
  putLayout.mockReset();
  getLayout.mockResolvedValue({ preset: "live", layout: [] });
  putLayout.mockResolvedValue({ preset: "live", layout: [] });
});

describe("RunCanvas — loading a layout", () => {
  it("renders the saved arrangement, in its saved positions", async () => {
    getLayout.mockResolvedValue({
      preset: "live",
      layout: [
        { widget: "terminal", x: 0, y: 6, w: 12, h: 6 },
        { widget: "identity", x: 0, y: 0, w: 12, h: 6 },
      ],
      updated_at: new Date().toISOString(),
    });

    render(<RunCanvas ctx={ctx()} />);

    // The saved layout WINS: the situational default places Egress, this one
    // does not.
    await waitFor(() => expect(widgetHeading("Egress")).not.toBeInTheDocument());
    expect(widgetHeading("Identity")).toBeInTheDocument();

    // ...and it wins on POSITION too, not just membership: the default puts the
    // terminal at the top, this layout puts Identity above it.
    await waitFor(() =>
      expect(tileY(screen.getByTestId("run-terminal-pane"))).toBeGreaterThan(
        tileY(widgetHeading("Identity")),
      ),
    );
  });

  it("falls through to the preset default when nothing is saved", async () => {
    render(<RunCanvas ctx={ctx()} />);

    expect(await screen.findByRole("heading", { name: "Egress" })).toBeInTheDocument();
    expect(widgetHeading("Sandbox")).toBeInTheDocument();
    expect(getLayout).toHaveBeenCalledWith("live");
    // The hero is at the top of the canvas — the phase-1 promise the e2e
    // asserts geometrically.
    expect(tileY(screen.getByTestId("run-terminal-pane"))).toBeLessThan(
      tileY(widgetHeading("Egress")) + 1,
    );
  });

  it("uses the finished preset for a stopped run — the situation picks it", async () => {
    render(<RunCanvas ctx={ctx({ finished: true })} />);

    await waitFor(() => expect(getLayout).toHaveBeenCalledWith("finished"));
    // A stopped run has no sandbox to meter, so that preset leaves it out.
    await waitFor(() => expect(widgetHeading("Sandbox")).not.toBeInTheDocument());
    expect(widgetHeading("Files changed")).toBeInTheDocument();
  });

  it("still renders a cockpit when the layout GET fails outright", async () => {
    getLayout.mockRejectedValue(new HttpError(500, "boom"));
    render(<RunCanvas ctx={ctx()} />);
    expect(await screen.findByRole("heading", { name: "Egress" })).toBeInTheDocument();
    expect(screen.getByTestId("run-terminal-pane")).toBeInTheDocument();
  });
});

describe("RunCanvas — saving", () => {
  it("PUTs the preset and the placed widgets in wire shape", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx()} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.saveLayoutDefault }));

    expect(putLayout).toHaveBeenCalledTimes(1);
    const [preset, layout] = putLayout.mock.calls[0] as [string, Record<string, number>[]];
    expect(preset).toBe("live");
    expect(layout).toContainEqual({ widget: "terminal", x: 0, y: 0, w: 8, h: 12 });
    expect(layout.every((w) => Object.keys(w).sort().join() === "h,w,widget,x,y")).toBe(true);
  });

  it("degrades to this session on a 501, with no error and no retry storm", async () => {
    const user = userEvent.setup();
    putLayout.mockRejectedValue(new HttpError(501, "layout persistence is not implemented"));
    render(<RunCanvas ctx={ctx()} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.saveLayoutDefault }));

    // Said once, in place — never a toast per drag.
    expect(await screen.findByText(RUN_COCKPIT.layoutNotPersisted)).toBeInTheDocument();

    // The canvas keeps working, and a further edit does NOT try the doomed
    // write again.
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.saveLayoutDefault }));
    expect(putLayout).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("run-terminal-pane")).toBeInTheDocument();
  });
});

describe("RunCanvas — the catalog", () => {
  it("takes a widget off the canvas and puts it back", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx()} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.addWidget }));
    const catalog = await screen.findByRole("dialog");

    await user.click(within(catalog).getByRole("button", { name: "Sandbox" }));
    await waitFor(() => expect(widgetHeading("Sandbox")).not.toBeInTheDocument());

    await user.click(within(catalog).getByRole("button", { name: "Sandbox" }));
    await waitFor(() => expect(widgetHeading("Sandbox")).toBeInTheDocument());
  });
});

describe("RunCanvas — the terminal cannot be arranged away", () => {
  it("refuses to remove it from the catalog, and offers no tile remove", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx()} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    // Every other tile carries one; the terminal's does not.
    expect(screen.getByRole("button", { name: "Remove Egress" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove Terminal" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.addWidget }));
    const catalog = await screen.findByRole("dialog");
    expect(within(catalog).getByRole("button", { name: "Terminal" })).toBeDisabled();
  });

  it("puts it back when a saved layout arrives without it", async () => {
    getLayout.mockResolvedValue({
      preset: "live",
      layout: [{ widget: "identity", x: 0, y: 0, w: 12, h: 6 }],
      updated_at: new Date().toISOString(),
    });
    render(<RunCanvas ctx={ctx()} />);

    await waitFor(() => expect(widgetHeading("Egress")).not.toBeInTheDocument());
    expect(screen.getByTestId("run-terminal-pane")).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// T2b/M3 — terminal-first. Every one of these already HELD when they were
// written; they exist because nothing pinned them, so a preset edit could have
// quietly demoted the session to a tile among tiles and no test would have
// noticed. "Dominant" is the load-bearing word: at the top is not enough.
// ---------------------------------------------------------------------------
describe("the live preset is terminal-first", () => {
  const area = (w: { w: number; h: number }) => w.w * w.h;

  it("gives the terminal the whole viewport height and most of its width", () => {
    const term = presetLayout("live").find((w) => w.widget === "terminal")!;
    expect(term).toMatchObject({ x: 0, y: 0 });
    // GRID_ROWS is one viewport (canvas.tsx derives rowHeight from it), so a
    // full-height hero is a hero that never needs scrolling to.
    expect(term.h).toBe(GRID_ROWS);
    expect(term.w).toBeGreaterThan(GRID_COLS / 2);
  });

  it("makes it the biggest tile on the canvas, not merely the first", () => {
    const live = presetLayout("live");
    const term = live.find((w) => w.widget === "terminal")!;
    for (const other of live.filter((w) => w.widget !== "terminal")) {
      expect(area(other)).toBeLessThan(area(term));
    }
  });

  it("an interactive RUNNING run lands on it — the run you drive is the run that needs the pixels", async () => {
    render(<RunCanvas ctx={ctx({ run: { ...RUN, interactive: true } as WidgetContext["run"] })} />);
    await waitFor(() => expect(getLayout).toHaveBeenCalledWith("live"));
    // Interactive vs autonomous is the Terminal WIDGET's own state, never a
    // third preset — so the arrangement is the same live one either way.
    expect(await screen.findByRole("heading", { name: "Egress" })).toBeInTheDocument();
    expect(tileY(screen.getByTestId("run-terminal-pane"))).toBeLessThan(
      tileY(widgetHeading("Egress")) + 1,
    );
  });

  it("still yields to a saved layout — terminal-first is the default, not a rule", async () => {
    getLayout.mockResolvedValue({
      preset: "live",
      layout: [
        { widget: "identity", x: 0, y: 0, w: 12, h: 6 },
        { widget: "terminal", x: 0, y: 6, w: 12, h: 6 },
      ],
      updated_at: new Date().toISOString(),
    });
    render(<RunCanvas ctx={ctx({ run: { ...RUN, interactive: true } as WidgetContext["run"] })} />);
    await waitFor(() =>
      expect(tileY(screen.getByTestId("run-terminal-pane"))).toBeGreaterThan(
        tileY(widgetHeading("Identity")),
      ),
    );
  });
});
