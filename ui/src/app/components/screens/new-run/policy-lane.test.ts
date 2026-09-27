/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1200 — combineFloors and barrierRequirementReason: the pure math behind
// folding a governance ceiling's floor into New Run's Barrier control.
import { describe, expect, it } from "vitest";
import { CC_ORDER, type ConfinementClass } from "../../../lib/types";
import { barrierReasons, barrierRequirementReason, combineFloors, governanceRemovedTier } from "./policy-lane";

describe("combineFloors", () => {
  it("with only an authored floor, that floor wins", () => {
    expect(combineFloors("CC2", undefined)).toBe("CC2");
  });

  it("with only a governance floor, that floor wins", () => {
    expect(combineFloors(undefined, "CC3")).toBe("CC3");
  });

  it("with neither, there is no floor at all", () => {
    expect(combineFloors(undefined, undefined)).toBeUndefined();
  });

  it("the STRONGER of the two always wins, whichever side it's on — composer.Clamp raises, never lowers", () => {
    expect(combineFloors("CC1", "CC3")).toBe("CC3");
    expect(combineFloors("CC3", "CC1")).toBe("CC3");
    expect(combineFloors("CC2", "CC2")).toBe("CC2");
  });
});

describe("barrierRequirementReason", () => {
  it("names the tier as not installed when it's the missing one", () => {
    expect(barrierRequirementReason("CC3", ["CC2", "CC3"], [])).toBe(
      "Vault isn't installed on this host.",
    );
  });

  it("names the general shortfall when the floor tier IS installed but every tier here ranks below it", () => {
    // Unusual (the floor tier itself isn't in `unavailable`), but stay honest
    // rather than claim "isn't installed" about a tier that is.
    expect(barrierRequirementReason("CC2", [], ["CC1"])).toBe(
      "Every barrier installed on this host is below Wall.",
    );
  });

  // #1200 review P2-6 — Vault gets the SAME honest /dev/kvm reason
  // environment-step.tsx's own picker computes, not a generic "isn't
  // installed", whenever the caller has one to pass.
  it("prefers the caller's vault reason over the generic line, for Vault only", () => {
    expect(barrierRequirementReason("CC3", ["CC3"], [], "no /dev/kvm here.")).toBe(
      "no /dev/kvm here.",
    );
    // Never applied to a non-Vault floor.
    expect(barrierRequirementReason("CC2", ["CC2"], [], "no /dev/kvm here.")).toBe(
      "Wall isn't installed on this host.",
    );
  });

  it("falls back to the generic line when no vault reason was given (undefined or empty)", () => {
    expect(barrierRequirementReason("CC3", ["CC3"], [], undefined)).toBe(
      "Vault isn't installed on this host.",
    );
    expect(barrierRequirementReason("CC3", ["CC3"], [], "")).toBe(
      "Vault isn't installed on this host.",
    );
  });
});

// #1200 review R2-1 — "set by your admin" only when the governance floor
// actually removed an installed tier. Exhaustive over every installed set
// (including unknown), authored floor and governance floor, against an
// independent oracle: the tiers the run could use under its own floor alone,
// minus the tiers it can use once the governance floor is folded in.
describe("governanceRemovedTier", () => {
  const floors: Array<ConfinementClass | undefined> = [undefined, ...CC_ORDER];
  const installedSets: Array<ConfinementClass[] | null> = [
    null,
    ...Array.from({ length: 8 }, (_, m) => CC_ORDER.filter((_, i) => m & (1 << i))),
  ];

  it("matches the set-difference oracle for all 9 × 4 × 4 combinations", () => {
    let checked = 0;
    for (const installed of installedSets) {
      for (const authored of floors) {
        for (const governance of floors) {
          const own = barrierReasons(installed, authored).qualifying ?? [];
          const governed = barrierReasons(installed, combineFloors(authored, governance)).qualifying ?? [];
          const removed = own.some((c) => !governed.includes(c));
          const label = JSON.stringify({ installed, authored, governance });
          expect(governanceRemovedTier(installed, authored, governance), label).toBe(removed);
          checked++;
        }
      }
    }
    expect(checked).toBe(144);
  });

  it("P2b: unassigned member (deployment floor Fence) on a Fence-only host — nothing removed", () => {
    expect(governanceRemovedTier(["CC1"], "CC1", "CC1")).toBe(false);
  });

  it("P2c: a Wall floor on a Wall-only host — nothing removed", () => {
    expect(governanceRemovedTier(["CC2"], "CC1", "CC2")).toBe(false);
  });

  it("a Vault floor on a full host removes Fence and Wall", () => {
    expect(governanceRemovedTier(["CC1", "CC2", "CC3"], "CC1", "CC3")).toBe(true);
  });

  it("unknown availability claims nothing", () => {
    expect(governanceRemovedTier(null, "CC1", "CC3")).toBe(false);
  });
});
