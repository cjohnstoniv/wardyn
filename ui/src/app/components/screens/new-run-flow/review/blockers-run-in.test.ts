/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { ElevationOffer } from "../../../../lib/types/governance-requests";
import { NR_RUN_IN } from "../../../wardyn/copy/new-run/run-in";
import { buildBlockers, groupsBlockedBy, type BlockerInput } from "./blockers";

const OFFER: ElevationOffer = { unit_id: "u-bill", unit_name: "Billing", max_elevation: "4h" };
const SERVER_SENTENCE = "The unit you asked to run in is not one of your units";
const REFUSAL = { text: SERVER_SENTENCE, reason: "unit_not_a_context" };

const base = (over: Partial<BlockerInput> = {}): BlockerInput => ({
  issues: [], unsupported: [], templateNeeds: [], refusal: null, setupItems: [], stale: false, ...over,
});

describe("Review blockers: a unit that is only offered", () => {
  it("asks for access, with Request elevation for that unit", () => {
    const out = buildBlockers(base({ runIn: { kind: "ask", unitId: "u-bill", name: "Billing", offer: OFFER } }));
    expect(out).toEqual([
      {
        id: "run_in:ask", kind: "refusal", text: NR_RUN_IN.BLOCKER_ASK("Billing"), to: { screen: "step", step: "run" }, field: "run_in",
        requestElevation: { unitId: "u-bill", offer: OFFER },
      },
    ]);
    expect(NR_RUN_IN.BLOCKER_ASK("Billing")).toBe("Billing needs access you don't have. Ask for time-limited access to run in it.");
    expect(NR_RUN_IN.REQUEST_ELEVATION).toBe("Request elevation");
  });

  it("says the request waits, with no button", () => {
    const out = buildBlockers(base({ runIn: { kind: "waiting", unitId: "u-bill", name: "Billing" } }));
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({ id: "run_in:waiting", kind: "refusal", text: NR_RUN_IN.BLOCKER_WAITING("Billing"), field: "run_in" });
    expect(out[0].requestElevation).toBeUndefined();
  });

  it("marks the Where group of the run", () => {
    const out = buildBlockers(base({ runIn: { kind: "ask", unitId: "u-bill", name: "Billing", offer: OFFER } }));
    expect([...groupsBlockedBy(out)]).toEqual(["where"]);
  });

  it("does not list the server's unit_not_a_context refusal a second time", () => {
    for (const runIn of [{ kind: "ask", unitId: "u-bill", name: "Billing", offer: OFFER }, { kind: "waiting", unitId: "u-bill", name: "Billing" }] as const) {
      const out = buildBlockers(base({ runIn, refusal: REFUSAL }));
      expect(out).toHaveLength(1);
      expect(out[0].id).toMatch(/^run_in:/);
    }
  });

  it("keeps the server's sentence for that refusal when no unit is offered", () => {
    const out = buildBlockers(base({ refusal: REFUSAL }));
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({ id: "refusal", kind: "refusal", text: SERVER_SENTENCE });
  });

  it("keeps another refusal beside the blocker", () => {
    const out = buildBlockers(base({ runIn: { kind: "ask", unitId: "u-bill", name: "Billing", offer: OFFER }, refusal: { text: "Other.", reason: "something_else" } }));
    expect(out.map((b) => b.id)).toEqual(["run_in:ask", "refusal"]);
  });
});
