/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { LIMITS_DIALOG, MY_POOLS } from "./runner-pools";

// "Runs at once" is a field label, so a sentence that names the setting quotes it. The label itself does not.
describe("sentences that name the Runs at once setting", () => {
  it("quote it in the formula lines and the whole-number error", () => {
    expect(LIMITS_DIALOG.CALC(24, 6, 4, 96, 16, 1000, 166)).toContain('Total ÷ "Runs at once":');
    expect(LIMITS_DIALOG.CALC_PENDING).toContain('Total ÷ "Runs at once" is');
    expect(LIMITS_DIALOG.USE_SHARE).toBe('Use total ÷ "Runs at once"');
    expect(LIMITS_DIALOG.ERR_RUNS).toBe('"Runs at once" is a whole number, 1 or more.');
  });
  it("quotes it in the suggestion sentence", () => {
    expect(MY_POOLS.SUGGESTED("4 CPUs", 4, "x")).toContain('÷ "Runs at once" (');
  });
  it("leaves the field's own label bare", () => {
    expect(LIMITS_DIALOG.RUNS_AT_ONCE).toBe("Runs at once");
  });
});
