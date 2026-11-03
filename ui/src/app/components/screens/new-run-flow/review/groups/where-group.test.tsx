/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MAC_BASES } from "../../../../wardyn/copy/mac-bases";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_WINDOW } from "../../../../wardyn/copy/new-run/window";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { MacOSOfferContext, type MacOSOffer } from "../../steps/run/macos-offer";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../../../../../lib/new-run-fixtures/scenarios/preflight";
import type { DraftAction } from "../../draft/draft-reducer";
import { initialDraft, type RunIntent } from "../../draft/run-intent";
import { BlockedGroups } from "../review-group";
import { WhereGroup } from "./where-group";

vi.mock("../../steps/run/pool-source", async (orig) => ({
  ...(await orig<typeof import("../../steps/run/pool-source")>()),
  loadPools: () => Promise.resolve(SCENARIO_POOLS.member),
}));

afterEach(cleanup);

const AGENT: RunIntent = { shape: "agent_task", experience: "interactive" };

function renderWhere(intent: RunIntent = AGENT) {
  const dispatch = vi.fn<(a: DraftAction) => void>();
  const draft = { ...initialDraft("alice"), intent };
  render(<WhereGroup draft={draft} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={dispatch} />);
  /** What the draft would become after every edit dispatched so far. */
  const edited = () => dispatch.mock.calls.reduce((i, [a]) => (a.type === "edit" ? a.apply(i) : i), AGENT);
  return { edited };
}

describe("Review: where it runs, Change", () => {
  it("edits the pool and the barrier in place, then Done closes the editor", async () => {
    const user = userEvent.setup();
    const { edited } = renderWhere();
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WHERE}` }));
    const pool = await screen.findByLabelText(NR_REVIEW.POOL_LABEL);
    const barrier = screen.getByLabelText(NR_REVIEW.BARRIER_LABEL);
    await vi.waitFor(() => expect(pool).toBeEnabled());
    expect(pool).toHaveValue(SCENARIO_POOLS.member.pools[0].id);
    await user.selectOptions(barrier, "CC3");
    expect(edited().confinement_class).toBe("CC3");
    await user.click(screen.getByRole("button", { name: NR_REVIEW.DONE }));
    expect(screen.queryByLabelText(NR_REVIEW.BARRIER_LABEL)).toBeNull();
    expect(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WHERE}` })).toHaveAttribute("aria-expanded", "false");
  });

  it("shows a Linux VM's fixed barrier, as the Run step does, and no container barrier select", async () => {
    const user = userEvent.setup();
    renderWhere({ ...AGENT, machine_kind: "linux_vm" });
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WHERE}` }));
    await screen.findByLabelText(NR_REVIEW.POOL_LABEL);
    expect(screen.queryByLabelText(NR_REVIEW.BARRIER_LABEL)).toBeNull();
    expect(screen.getByTestId("nr-review-vm-barrier")).toHaveTextContent(NR_MACHINE.BARRIER_VM);
  });
});

// #3043: the pool-required blocker points at this row, so the row says there is none instead of vanishing.
describe("Review: where it runs with no pool", () => {
  const draft = { ...initialDraft("alice"), intent: AGENT };
  const view = (blocked: boolean) =>
    render(
      <BlockedGroups.Provider value={new Set(blocked ? (["where"] as const) : [])}>
        <WhereGroup draft={draft} pre={null} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />
      </BlockedGroups.Provider>,
    );

  it("reads 'No pool chosen yet', muted, when a blocker points here and no pool is known", () => {
    view(true);
    expect(screen.getByText("No pool chosen yet").className).toContain("text-muted-foreground");
    expect(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WHERE}` })).toBeInTheDocument();
  });

  it("says nothing while no blocker points here (the read is still pending)", () => {
    view(false);
    expect(screen.queryByText("No pool chosen yet")).toBeNull();
  });
});

// Board J6, "Review row": a macOS VM on another base names it under Where it runs; Standard names nothing.
describe("Review: where it runs on a macOS base", () => {
  const view = (intent: RunIntent) =>
    render(<WhereGroup draft={{ ...initialDraft("alice"), intent }} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);

  it("puts MAC-B16 on its own line under the pool's detail for a chosen base", () => {
    view({ ...AGENT, machine_kind: "macos_vm", macos_base: "xcode-16" });
    const line = screen.getByText(MAC_BASES.B16("xcode-16"));
    expect(line.tagName).toBe("SPAN");
    expect(line.parentElement?.textContent).toMatch(/\S.*macOS base · xcode-16$/);
    expect(line.parentElement?.textContent).not.toBe(MAC_BASES.B16("xcode-16"));
  });
  it("names no base for Standard, or for a draft that is no longer a macOS VM", () => {
    view({ ...AGENT, machine_kind: "macos_vm" });
    view({ ...AGENT, machine_kind: "linux_vm", macos_base: "xcode-16" });
    expect(screen.queryByText((t) => t.includes("macOS base ·"))).toBeNull();
  });
});

// Board WIN-2, Review: one line when a window on the Mac is chosen, a cleared choice where it was made, and the
// checkbox in the Change editor in place of a macOS VM's barrier.
describe("Review: where it runs, the window on the Mac", () => {
  const ON = { features: { macos_vms: true, mac_window: true } } as SetupStatus;
  const ONE_MAC: MacOSOffer = { ok: true, guestOS: "macOS 26.1", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"], runner: "Maya's MacBook" };
  const MAC: RunIntent = { ...AGENT, machine_kind: "macos_vm" };
  const view = (intent: RunIntent, opts: { offer?: MacOSOffer; status?: SetupStatus | null; cleared?: string } = {}) => {
    const dispatch = vi.fn<(a: DraftAction) => void>();
    const draft = { ...initialDraft("alice"), intent, guided: { ...initialDraft("alice").guided, windowCleared: opts.cleared } };
    render(
      <ModelAccessProvider status={opts.status === undefined ? ON : opts.status} onRefresh={() => {}}>
        <MacOSOfferContext.Provider value={opts.offer}>
          <WhereGroup draft={draft} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={dispatch} />
        </MacOSOfferContext.Provider>
      </ModelAccessProvider>,
    );
    return dispatch;
  };

  it("chosen on a pool of one Mac: W2-07 under the pool's detail (w2-review-on)", () => {
    view({ ...MAC, desktop_window: "at_start" }, { offer: ONE_MAC });
    expect(screen.getByText(NR_WINDOW.REVIEW_ONE_MAC("Maya's MacBook")).tagName).toBe("SPAN");
  });

  it("chosen on a pool of several Macs: W2-08 (w2-review-several)", () => {
    view({ ...MAC, desktop_window: "at_start" }, { offer: { ...ONE_MAC, runner: undefined } });
    expect(screen.getByText(NR_WINDOW.REVIEW_SEVERAL_MACS)).toBeInTheDocument();
  });

  it("not chosen: no line, exactly as before (w2-review-off)", () => {
    view(MAC, { offer: ONE_MAC });
    expect(screen.queryByText(/^Window on /)).toBeNull();
    expect(screen.queryByText(NR_WINDOW.CLEARED)).toBeNull();
  });

  it("cleared by the server: W2-06 and the server's sentence where it was chosen (w2-f1-after)", () => {
    const why = "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead.";
    view(MAC, { offer: ONE_MAC, cleared: why });
    expect(screen.getByText(NR_WINDOW.CLEARED)).toBeInTheDocument();
    expect(screen.getByText(why)).toBeInTheDocument();
    expect(screen.queryByText(/^Window on /)).toBeNull();
  });

  it("Change: the window checkbox takes the barrier's place for a macOS VM, and ticking it writes desktop_window (w2-review-editor)", async () => {
    const user = userEvent.setup();
    const dispatch = view(MAC, { offer: ONE_MAC });
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WHERE}` }));
    expect(screen.queryByLabelText(NR_REVIEW.BARRIER_LABEL)).toBeNull();
    expect(screen.getByRole("group", { name: NR_WINDOW.GROUP })).toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: NR_WINDOW.OPTION }));
    const edited = dispatch.mock.calls.reduce((i, [a]) => (a.type === "edit" ? a.apply(i) : i), MAC);
    expect(edited.desktop_window).toBe("at_start");
    expect(dispatch.mock.calls.some(([a]) => a.type === "guided" && "windowCleared" in a.patch && a.patch.windowCleared === undefined)).toBe(true);
  });

  it("Change with the site switch off shows the Run step's fixed VM barrier, no container barrier select and no window checkbox", async () => {
    const user = userEvent.setup();
    view(MAC, { offer: ONE_MAC, status: { features: { macos_vms: true } } as SetupStatus });
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WHERE}` }));
    expect(screen.queryByLabelText(NR_REVIEW.BARRIER_LABEL)).toBeNull();
    expect(screen.getByTestId("nr-review-vm-barrier")).toHaveTextContent(NR_MACHINE.BARRIER_VM);
    expect(screen.queryByRole("checkbox", { name: NR_WINDOW.OPTION })).toBeNull();
  });
});
