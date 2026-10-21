/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { MY_POOLS, POOL_FORM, RUNNER_V6 } from "./runner-pools";
import { MY_RUNNER, RUNNERS } from "./runners";

// #2895: "Runs at once" is a field label, so prose that points at it quotes it.
describe("prose that names the Runs at once limit", () => {
  const limit = 'up to its "Runs at once" limit';
  it("quotes the field name in every sentence that points at it", () => {
    expect(RUNNERS.LEAD).toContain(limit);
    expect(MY_RUNNER.LEDE).toContain(limit);
    expect(RUNNER_V6.MINT_ONE_PER_COMPUTER).toContain(limit);
    expect(MY_POOLS.REGISTER_ONE_PER_COMPUTER).toContain(limit);
    expect(POOL_FORM.MAX_RUNS_HINT_NEW).toContain('"Runs at once" limits');
  });
  it("leaves no bare 'its Runs at once' in those strings", () => {
    for (const s of [RUNNERS.LEAD, MY_RUNNER.LEDE, RUNNER_V6.MINT_ONE_PER_COMPUTER, MY_POOLS.REGISTER_ONE_PER_COMPUTER, POOL_FORM.MAX_RUNS_HINT_NEW]) {
      expect(s).not.toMatch(/its Runs at once|runners' Runs at once/);
    }
  });
});
