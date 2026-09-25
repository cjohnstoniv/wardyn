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
import { MemoryRouter } from "react-router-dom";

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

// The ssh tile's card fetches these on mount (the admin-non-owner case below
// is the only test here that reaches it).
vi.mock("../../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({ ssh: { enabled: true, advertise_addr: "ssh.example:2222" } }) },
}));
vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: vi.fn().mockResolvedValue([{ id: "k1", name: "laptop", fingerprint: "SHA256:x" }]) },
}));
// The save outcome is SPOKEN, not rendered: two of the three arms only ever
// reach a toast, and nothing mounts a <Toaster/> here. Spy on the three calls
// so "which sentence did the operator get" is assertable at all.
const toasted = vi.hoisted(() => ({
  success: vi.fn(),
  message: vi.fn(),
  error: vi.fn(),
}));
vi.mock("sonner", () => ({
  toast: {
    success: (...a: unknown[]) => toasted.success(...a),
    message: (...a: unknown[]) => toasted.message(...a),
    error: (...a: unknown[]) => toasted.error(...a),
  },
}));

import { HttpError } from "../../../lib/api/core";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { RunCanvas } from "./canvas";
import { OperatorProvider } from "../../wardyn/operator-context";
import { GRID_COLS, GRID_ROWS, RUN_WIDGETS, presetLayout, type WidgetContext } from "./widget-registry";

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
    // A non-owner, non-admin viewer keeps the ssh widget unavailable, so this
    // suite never has to stand up the health / ssh-key fetches.
    principal: null,
    operator: false,
    view: "user",
    grants: [],
    egress: [],
    heldCount: 0,
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

// When a run finishes DURING canvas editing, `situational` flips — the
// effect must stay guarded rather than setPreset(situational) on every flip,
// or `dirty=false` + commit() would replace the in-progress arrangement on
// screen (the server keeps the edit; only the screen would jump).
describe("RunCanvas — a situational flip mid-edit does not clobber the in-progress arrangement", () => {
  // ticket: F1-F5
  it("freezes the preset while editing, and re-syncs the moment editing ends", async () => {
    const user = userEvent.setup();
    const { rerender } = render(<RunCanvas ctx={ctx({ finished: false })} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));

    // The run finishes mid-edit — situational flips to "finished".
    rerender(<RunCanvas ctx={ctx({ finished: true })} />);
    // R-2: `Egress` is in BOTH presets, so its mere presence proves nothing —
    // the preset itself must not have followed. useRunLayout's own fetch
    // effect is keyed on `preset` and calls `getLayout` SYNCHRONOUSLY within
    // the same effect flush the instant preset changes, so this is
    // timing-safe (unlike asserting on the fetch's still-async RESULT).
    expect(getLayout).not.toHaveBeenCalledWith("finished");
    expect(screen.getByRole("heading", { name: "Egress" })).toBeInTheDocument();

    // Exiting edit still re-syncs to the run's actual situation.
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.doneEditing }));
    await waitFor(() => expect(getLayout).toHaveBeenCalledWith("finished"));
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Sandbox" })).not.toBeInTheDocument(),
    );
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

// M3 — terminal-first. Every one of these already HELD when they were
// written; they exist because nothing pinned them, so a preset edit could have
// quietly demoted the session to a tile among tiles and no test would have
// noticed. "Dominant" is the load-bearing word: at the top is not enough.
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

// The ssh tile's availability predicate must mirror ConnectSSHCard's own
// gate — the card is owner OR ADMIN (run-detail-ssh.tsx:50,
// docs/design/ui-sandboxes-prompt.md's 0.6 amendment, and
// run-detail-ssh.test.tsx's passing admin case), not owner only. `available`
// is what canvas.tsx and focus-mode.tsx filter on, so an admin's cockpit must
// not drop the tile on a run they did not start — even when their SAVED
// layout named it. This suite's default ctx is a non-owner non-admin, which
// is exactly why a regression here needs its own coverage.
describe("RunCanvas — the SSH tile follows the card's own owner-OR-admin gate", () => {
  const adminCtx = () =>
    ctx({
      operator: true,
      principal: "admin@example.com",
      run: { ...RUN, created_by: "someone-else@example.com", state: "RUNNING" } as WidgetContext["run"],
    });

  it("says the tile is available to an admin on a run they do not own", () => {
    expect(RUN_WIDGETS.ssh.available?.(adminCtx())).toBe(true);
  });

  it("keeps it away from a viewer who is neither the owner nor an admin", () => {
    expect(
      RUN_WIDGETS.ssh.available?.(
        ctx({
          operator: false,
          principal: "bob@example.com",
          run: { ...RUN, created_by: "someone-else@example.com", state: "RUNNING" } as WidgetContext["run"],
        }),
      ),
    ).toBe(false);
  });

  // M-7 (admin-member-modes-design.md §4.6, §6) — the admin monitor carries
  // no Connect-via-SSH door at all, even for a super admin on their OWN run.
  it("keeps it away from the admin view, even on the admin's own run", () => {
    expect(
      RUN_WIDGETS.ssh.available?.(
        ctx({
          view: "admin",
          operator: true,
          principal: "admin@example.com",
          run: { ...RUN, created_by: "admin@example.com", state: "RUNNING" } as WidgetContext["run"],
        }),
      ),
    ).toBe(false);
  });

  it("actually places it on the canvas — a saved layout that names it is honoured", async () => {
    getLayout.mockResolvedValue({
      preset: "live",
      layout: [
        { widget: "terminal", x: 0, y: 0, w: 8, h: 12 },
        { widget: "ssh", x: 8, y: 0, w: 4, h: 5 },
      ],
      updated_at: new Date().toISOString(),
    });
    // The card reads useOperator itself and renders a <Link>, so it needs the
    // same two providers the shell gives it.
    render(
      <MemoryRouter>
        <OperatorProvider operator principal="admin@example.com">
          <RunCanvas ctx={adminCtx()} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(
      await screen.findByRole("heading", { name: RUN_WIDGETS.ssh.label }),
    ).toBeInTheDocument();
  });
});

// A late layout GET must never undo an arrangement already made: on a slow
// control plane a drag/remove/add made WHILE the GET is in flight must
// survive on screen — the debounced PUT (which closes over the human's
// layout) still writes that edit to the server, so screen and server must
// agree rather than disagree until the next page load, with no message
// either way.
describe("RunCanvas — a late layout GET never undoes an arrangement already made", () => {
  it("keeps the removed tile removed when the slow GET finally answers", async () => {
    const user = userEvent.setup();
    let answer: (v: unknown) => void = () => {};
    getLayout.mockReturnValue(
      new Promise((r) => {
        answer = r;
      }),
    );
    render(<RunCanvas ctx={ctx()} />);

    // The preset default paints immediately, so the human can arrange before
    // the server has said anything.
    await user.click(await screen.findByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: "Remove Egress" }));
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Egress" })).toBeNull());

    // ...and now the GET lands with the arrangement saved BEFORE the gesture.
    answer({ preset: "live", layout: presetLayout("live"), updated_at: new Date().toISOString() });

    await waitFor(() => expect(screen.getByTestId("run-terminal-pane")).toBeInTheDocument());
    expect(screen.queryByRole("heading", { name: "Egress" })).toBeNull();
  });
});


// The catalog must offer only what `available` admits, matching what the
// grid actually draws — offering "Attach from your terminal" on a run where
// the ssh gate is false (every finished run, and every run you did not
// start) would let one click tick it, run addWidget and PUT a phantom
// placement, with no tile ever appearing. focus-mode.tsx's dock already
// filters on the same predicate; the catalog must too.
describe("RunCanvas — the catalog offers only what can actually render", () => {
  const finishedOwnerCtx = () =>
    ctx({
      finished: true,
      principal: "me",
      run: { ...RUN, state: "SUCCEEDED" } as WidgetContext["run"],
    });

  it("leaves an unavailable widget out of the Add-widget list entirely", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={finishedOwnerCtx()} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.addWidget }));
    const catalog = await screen.findByRole("dialog");

    expect(
      within(catalog).queryByRole("button", { name: RUN_WIDGETS.ssh.label }),
    ).not.toBeInTheDocument();
    // ...and the entries that CAN render are still all there.
    expect(within(catalog).getByRole("button", { name: "Sandbox" })).toBeInTheDocument();
    // Nothing was persisted by merely opening it.
    expect(putLayout).not.toHaveBeenCalled();
  });

  it("still offers it once the same widget is renderable", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx({ principal: "me" })} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.addWidget }));
    const catalog = await screen.findByRole("dialog");

    expect(within(catalog).getByRole("button", { name: RUN_WIDGETS.ssh.label })).toBeInTheDocument();
  });
});


// put() reports THREE outcomes and saveDefault must speak three different
// sentences from them, never collapsing "failed" back into "unsupported" —
// both arms need their own coverage, or a regression there would slip past
// every other run-detail test. The two sentences are opposites:
// layoutNotPersisted is a permanent fact about the deployment that also
// silences every future write (persistRef), layoutSaveFailed is about THIS
// attempt and the next one is expected to work. A 500 or a network blip must
// never take the permanent arm — that would wrongly tell an operator their
// server cannot store layouts, and stop writing for good.
describe("RunCanvas — a save that merely FAILED is not a deployment that cannot persist", () => {
  beforeEach(() => {
    toasted.success.mockReset();
    toasted.message.mockReset();
    toasted.error.mockReset();
  });

  it("says try-again on a 500, keeps persisting, and writes again on the next save", async () => {
    const user = userEvent.setup();
    putLayout.mockRejectedValue(new HttpError(500, "boom"));
    render(<RunCanvas ctx={ctx()} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.saveLayoutDefault }));

    await waitFor(() => expect(toasted.error).toHaveBeenCalledWith(RUN_COCKPIT.layoutSaveFailed));
    // The permanent claim is never made — neither as a toast nor as the
    // toolbar's own inline note, which is the same string and the same lie.
    expect(toasted.message).not.toHaveBeenCalled();
    expect(screen.queryByText(RUN_COCKPIT.layoutNotPersisted)).toBeNull();

    // Retryable means retried: the 501 arm stops writing forever, this one
    // must not.
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.saveLayoutDefault }));
    await waitFor(() => expect(putLayout).toHaveBeenCalledTimes(2));
  });

  it("keeps the two arms apart from the other side: a 501 never says try-again", async () => {
    const user = userEvent.setup();
    putLayout.mockRejectedValue(new HttpError(501, "layout persistence is not implemented"));
    render(<RunCanvas ctx={ctx()} />);
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.editLayout }));
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.saveLayoutDefault }));

    await waitFor(() => expect(toasted.message).toHaveBeenCalledWith(RUN_COCKPIT.layoutNotPersisted));
    expect(toasted.error).not.toHaveBeenCalled();
  });
});

// The tile's FILL rule targets a widget that renders ONE root card
// (`[&>section]:flex-1`); the terminal widget renders a FRAGMENT instead —
// the M7(b) failure block, then the pane, then the approvals strip — so the
// rule must not also catch the failure block, a `shrink-0` <section> written
// to size to its content. Letting it through gives the failure block
// `flex: 1 1 0%`, shrinking the replay pane by roughly half on every
// KILLED/FAILED run, down to a bordered card holding only an "Open audit
// trail" button when the ending is one this build does not recognise.
//
// jsdom computes no layout, so this asserts the SELECTOR rather than the pixels:
// take every `[&>SEL]:util` arbitrary variant off the elements that carry it
// and ask what SEL actually matches in the rendered canvas.
function fillTargets(root: HTMLElement): Element[] {
  const out: Element[] = [];
  root.querySelectorAll<HTMLElement>("[class*='[&>']").forEach((el) => {
    for (const m of (el.getAttribute("class") ?? "").matchAll(/\[&(>[^\]]+)\]:/g)) {
      out.push(...el.querySelectorAll(`:scope ${m[1]}`));
    }
  });
  return out;
}

describe("RunCanvas — the fill rule stretches a widget's root card, not a widget's siblings", () => {
  const paneWithFailureBlock = (
    <>
      {/* failure-block.tsx's shape: a shrink-0 section, FIRST, above the pane. */}
      <section className="shrink-0" data-testid="run-failure-block" aria-label="What happened">
        <button type="button">Open audit trail</button>
      </section>
      <div data-testid="the-session">the session</div>
    </>
  );

  it("leaves the failure block out of the fill rule's reach", async () => {
    render(<RunCanvas ctx={ctx({ terminalPane: paneWithFailureBlock })} />);
    const block = await screen.findByTestId("run-failure-block");
    expect(fillTargets(document.body)).not.toContain(block);
  });

  it("still stretches a widget that IS a single card", async () => {
    render(<RunCanvas ctx={ctx({ terminalPane: paneWithFailureBlock })} />);
    const card = (await screen.findByRole("heading", { name: "Egress" })).closest("section");
    expect(card).not.toBeNull();
    expect(fillTargets(document.body)).toContain(card);
  });
});
