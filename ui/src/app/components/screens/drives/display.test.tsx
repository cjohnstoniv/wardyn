/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two enforcement predicates, pinned on the input that had them disagreeing:
// ABSENT.
//
// enforcementGloss(undefined) folded to GLOSS["none"] and printed "nothing binds
// this size" under all three disk fields, while isUncappedEnforcement(undefined)
// returned false and withheld the matching warning for the same input. One of the
// two had to be total over "unknown", and the gloss was the wrong half: an absent
// word means an older daemon or an undetected runner, never an affirmative claim
// that the substrate binds nothing.
import { describe, it, expect } from "vitest";

import { DRIVES } from "../../../lib/user-drives-copy";
import { GLOSS, enforcementGloss, isUncappedEnforcement } from "./display";

describe("enforcement display helpers", () => {
  it("renders NO gloss for an absent word, and no warning either", () => {
    expect(enforcementGloss(undefined)).toBe("");
    expect(isUncappedEnforcement(undefined)).toBe(false);
    // The point of the pin: whatever the two answer, they must not answer as if
    // the word were `none` in one half and unknown in the other.
    expect(enforcementGloss(undefined)).not.toBe(DRIVES.ENFORCEMENT_NONE);
  });

  it("still glosses every word in the vocabulary, `none` included", () => {
    for (const word of Object.keys(GLOSS) as (keyof typeof GLOSS)[]) {
      expect(enforcementGloss(word)).toBe(GLOSS[word]);
    }
    expect(enforcementGloss("none")).toBe(DRIVES.ENFORCEMENT_NONE);
  });

  it("warns for `none` alone — every other word binds bytes somewhere", () => {
    expect(isUncappedEnforcement("none")).toBe(true);
    for (const word of ["filesystem", "request", "external", "eviction"] as const) {
      expect(isUncappedEnforcement(word)).toBe(false);
    }
  });
});
