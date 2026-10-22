/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
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

function renderWhere() {
  const dispatch = vi.fn<(a: DraftAction) => void>();
  const draft = { ...initialDraft("alice"), intent: AGENT };
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
