/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { DefaultPolicyContext } from "../../../../../lib/api/policies";
import type { ElevationOffer, GovernanceRequest, GovernanceRequestList } from "../../../../../lib/types/governance-requests";
import { NR_RUN_IN } from "../../../../wardyn/copy/new-run/run-in";
import { reconcileRunIn, runInBlocker, runInChoices, shown, withoutRunIn, withRunIn } from "./run-in-model";

const NOW = Date.now();
const iso = (minutes: number) => new Date(NOW + minutes * 60_000).toISOString();

const PLATFORM: DefaultPolicyContext = { unit_id: "u-plat", unit_name: "Platform", home: true };
const DATA: DefaultPolicyContext = { unit_id: "u-data", unit_name: "Data science", home: false };
const PAYMENTS: DefaultPolicyContext = { unit_id: "u-pay", unit_name: "Payments", home: false };
const BILLING: ElevationOffer = { unit_id: "u-bill", unit_name: "Billing", max_elevation: "4h" };

const request = (over: Partial<GovernanceRequest>): GovernanceRequest =>
  ({
    id: "c1",
    state: "applied",
    kind: "elevation",
    unit_id: "u-pay",
    unit_name: "Payments",
    duration: "4h",
    ends_at: iso(190),
    mine: true,
    decidable: false,
    deciders: [],
    expires_at: iso(1000),
    ...over,
  }) as GovernanceRequest;

const listOf = (requests: GovernanceRequest[], offered: ElevationOffer[] = []): GovernanceRequestList => ({ requests, offered, image_offered: false });

describe("run in choices", () => {
  it("groups the contexts, a live applied elevation and the offers", () => {
    const c = runInChoices([PLATFORM, DATA, PAYMENTS], listOf([request({})], [BILLING]), NOW);
    expect(c?.items.map((i) => [i.group, i.name])).toEqual([
      ["yours", "Platform"],
      ["yours", "Data science"],
      ["elevated", "Payments"],
      ["ask", "Billing"],
    ]);
    expect(c?.home).toMatchObject({ unitId: "u-plat", home: true });
    expect(c?.requestsRead).toBe(true);
    expect(c?.items.find((i) => i.group === "elevated")).toMatchObject({ endsAt: request({}).ends_at });
    expect(c?.items.find((i) => i.group === "ask")).toMatchObject({ max: "4h", offer: BILLING });
  });

  it("puts a unit whose elevation has lapsed under your units", () => {
    const c = runInChoices([PLATFORM, PAYMENTS], listOf([request({ ends_at: iso(-1) })]), NOW);
    expect(c?.items.map((i) => [i.group, i.name])).toEqual([["yours", "Platform"], ["yours", "Payments"]]);
  });

  it("ignores requests that are not the person's own applied elevations", () => {
    const rows = [request({ mine: false }), request({ state: "pending" }), request({ kind: "image" })];
    const c = runInChoices([PLATFORM, PAYMENTS], listOf(rows), NOW);
    expect(c?.items.every((i) => i.group === "yours")).toBe(true);
  });

  it("drops an offer for a unit already among the contexts", () => {
    const c = runInChoices([PLATFORM, PAYMENTS], listOf([], [{ ...BILLING, unit_id: "u-pay", unit_name: "Payments" }, BILLING]), NOW);
    expect(c?.items.filter((i) => i.group === "ask").map((i) => i.name)).toEqual(["Billing"]);
  });

  it("orders the home unit first, then names; elevations by their end", () => {
    const later = { ...PAYMENTS, unit_id: "u-z", unit_name: "Zeta" };
    const c = runInChoices([DATA, PLATFORM, PAYMENTS, later], listOf([request({ unit_id: "u-z", unit_name: "Zeta", ends_at: iso(300) }), request({ ends_at: iso(60) })]), NOW);
    expect(c?.items.map((i) => i.name)).toEqual(["Platform", "Data science", "Payments", "Zeta"]);
  });

  it("gives only your units when the requests could not be read", () => {
    for (const list of [null, undefined]) {
      const c = runInChoices([PLATFORM, DATA], list, NOW);
      expect(c?.items.map((i) => i.group)).toEqual(["yours", "yours"]);
      expect(c?.requestsRead).toBe(false);
    }
  });

  it("gives nothing for a server that sends no contexts", () => {
    expect(runInChoices(undefined, listOf([]), NOW)).toBeUndefined();
  });
});

describe("run in shown", () => {
  it("is false for one unit and nothing else", () => {
    expect(shown(runInChoices([PLATFORM], listOf([]), NOW))).toBe(false);
    expect(shown(undefined)).toBe(false);
  });
  it("is true for two units", () => {
    expect(shown(runInChoices([PLATFORM, DATA], listOf([]), NOW))).toBe(true);
  });
  it("is true for one unit and an offer", () => {
    expect(shown(runInChoices([PLATFORM], listOf([], [BILLING]), NOW))).toBe(true);
  });
  it("is true for one unit and a live elevation", () => {
    expect(shown(runInChoices([PLATFORM, PAYMENTS], listOf([request({})]), NOW))).toBe(true);
  });
});

describe("run in reconcile", () => {
  const c = runInChoices([PLATFORM, DATA], listOf([], [BILLING]), NOW);
  if (!c) throw new Error("choices");

  it("keeps an absent choice, a unit by id and a unit by exact name", () => {
    expect(reconcileRunIn(undefined, c, null, "Platform")).toEqual({ keep: true });
    expect(reconcileRunIn("u-data", c, null, "Platform")).toEqual({ keep: true });
    expect(reconcileRunIn("Data science", c, null, "Platform")).toEqual({ keep: true });
    expect(reconcileRunIn("u-bill", c, null, "Platform")).toEqual({ keep: true });
  });

  it("says the elevation ended when the requests hold an applied elevation for the missing unit", () => {
    const list = listOf([request({ ends_at: iso(-5) })]);
    expect(reconcileRunIn("u-pay", c, list, "Platform")).toEqual({ keep: false, notice: NR_RUN_IN.ENDED("Payments", "Platform") });
    expect(reconcileRunIn("Payments", c, list, "Platform")).toEqual({ keep: false, notice: NR_RUN_IN.ENDED("Payments", "Platform") });
    expect(NR_RUN_IN.ENDED("Payments", "Platform")).toBe("Your elevation in Payments ended, so this run goes to your home unit, Platform.");
  });

  it("says the person is no longer in the unit when no such request is held", () => {
    expect(reconcileRunIn("Gone unit", c, listOf([]), "Platform")).toEqual({ keep: false, notice: NR_RUN_IN.NO_LONGER_IN("Gone unit", "Platform") });
    expect(reconcileRunIn("u-pay", c, null, "Platform")).toEqual({ keep: false, notice: NR_RUN_IN.NO_LONGER_IN("u-pay", "Platform") });
    expect(NR_RUN_IN.NO_LONGER_IN("Data science", "Platform")).toBe("You're no longer in Data science, so this run goes to your home unit, Platform.");
  });

  it("does not take another person's request for the person's elevation", () => {
    const list = listOf([request({ mine: false })]);
    expect(reconcileRunIn("u-pay", c, list, "Platform")).toMatchObject({ keep: false, notice: NR_RUN_IN.NO_LONGER_IN("u-pay", "Platform") });
  });
});

describe("run in blocker", () => {
  const c = runInChoices([PLATFORM, PAYMENTS], listOf([request({})], [BILLING]), NOW);

  it("asks for an offered unit that is picked", () => {
    expect(runInBlocker("u-bill", c, listOf([], [BILLING]))).toEqual({ kind: "ask", unitId: "u-bill", name: "Billing", offer: BILLING });
  });

  it("waits while a request for the unit is pending", () => {
    const list = listOf([request({ unit_id: "u-bill", unit_name: "Billing", state: "pending", ends_at: undefined })], [BILLING]);
    expect(runInBlocker("u-bill", c, list)).toEqual({ kind: "waiting", unitId: "u-bill", name: "Billing" });
  });

  it("asks again after the request was rejected or expired", () => {
    for (const state of ["rejected", "expired"] as const) {
      const list = listOf([request({ unit_id: "u-bill", unit_name: "Billing", state, ends_at: undefined })], [BILLING]);
      expect(runInBlocker("u-bill", c, list)).toMatchObject({ kind: "ask", unitId: "u-bill" });
    }
  });

  it("gives none for an elevated or own unit, or no pick", () => {
    expect(runInBlocker("u-pay", c, listOf([request({})]))).toBeUndefined();
    expect(runInBlocker("u-plat", c, listOf([]))).toBeUndefined();
    expect(runInBlocker(undefined, c, listOf([]))).toBeUndefined();
    expect(runInBlocker("u-bill", undefined, null)).toBeUndefined();
  });
});

describe("run in on the intent", () => {
  it("sets the unit and takes it back out without leaving a key", () => {
    const set = withRunIn({ shape: "agent_task" }, "u-pay");
    expect(set.run_in).toBe("u-pay");
    expect(withoutRunIn(set)).toEqual({ shape: "agent_task" });
    expect("run_in" in withoutRunIn(set)).toBe(false);
  });
});
