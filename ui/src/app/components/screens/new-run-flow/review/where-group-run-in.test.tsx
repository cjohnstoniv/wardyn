/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { DefaultPolicyContext } from "../../../../lib/api/policies";
import type { GovernanceRequest, GovernanceRequestList } from "../../../../lib/types/governance-requests";
import { SCENARIO_POOLS } from "../../../../lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { NR_RUN_IN } from "../../../wardyn/copy/new-run/run-in";
import { weekdayClock } from "../../run-detail/run-ends-row";
import type { RunIntent } from "../draft/run-intent";
import { initialDraft } from "../draft/run-intent";
import { runInChoices } from "../steps/run/run-in-model";
import type { RunInRead } from "../steps/run/use-run-in";
import { WhereGroup } from "./groups/where-group";

const NOW = Date.now();
const ENDS = new Date(NOW + 190 * 60_000).toISOString();
const PLATFORM: DefaultPolicyContext = { unit_id: "u-plat", unit_name: "Platform", home: true };
const PAYMENTS: DefaultPolicyContext = { unit_id: "u-pay", unit_name: "Payments", home: false };
const LIST: GovernanceRequestList = {
  requests: [{ id: "c1", state: "applied", kind: "elevation", unit_id: "u-pay", unit_name: "Payments", ends_at: ENDS, mine: true, decidable: false, deciders: [] } as unknown as GovernanceRequest],
  offered: [],
  image_offered: false,
};

const state = vi.hoisted(() => ({ read: undefined as unknown as RunInRead }));
vi.mock("../steps/run/use-run-in", () => ({ useRunIn: () => state.read }));
vi.mock("../steps/run/pool-source", async (orig) => ({
  ...(await orig<typeof import("../steps/run/pool-source")>()),
  loadPools: () => Promise.resolve(SCENARIO_POOLS.member),
}));

afterEach(cleanup);

function renderWhere(intent: RunIntent) {
  state.read = { status: "ready", choices: runInChoices([PLATFORM, PAYMENTS], LIST, NOW), list: LIST, now: NOW, reload: vi.fn() };
  const draft = { ...initialDraft("alice"), intent };
  render(<WhereGroup draft={draft} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
}

describe("Review: where it runs, Run in", () => {
  it("names an elevated unit with the Elevated chip and when it ends", () => {
    renderWhere({ shape: "agent_task", experience: "interactive", run_in: "u-pay" });
    expect(screen.getByText(NR_RUN_IN.REVIEW_ROW("Payments"))).toBeInTheDocument();
    expect(screen.getByText(NR_RUN_IN.CHIP_ELEVATED)).toBeInTheDocument();
    expect(screen.getByText(NR_RUN_IN.REVIEW_ENDS(weekdayClock(ENDS)))).toBeInTheDocument();
    expect(NR_RUN_IN.REVIEW_ROW("Payments")).toBe("Run in: Payments");
    expect(NR_RUN_IN.REVIEW_ENDS("Sun 18:40")).toBe("ends Sun 18:40");
  });

  it("names one of the person's own units with no chip and no end", () => {
    renderWhere({ shape: "agent_task", experience: "interactive", run_in: "u-plat" });
    expect(screen.getByText(NR_RUN_IN.REVIEW_ROW("Platform"))).toBeInTheDocument();
    expect(screen.queryByText(NR_RUN_IN.CHIP_ELEVATED)).toBeNull();
  });

  it("adds nothing for a draft with no unit chosen", () => {
    renderWhere({ shape: "agent_task", experience: "interactive" });
    expect(screen.queryByText(/^Run in:/)).toBeNull();
    expect(screen.queryByText(NR_RUN_IN.CHIP_ELEVATED)).toBeNull();
  });
});
