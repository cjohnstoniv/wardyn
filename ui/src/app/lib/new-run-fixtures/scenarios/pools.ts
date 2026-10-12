/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunnerPoolChoice } from "../../types/runner-pools";
import { everyScenario, type ByScenario } from "./index";

const SHARED: RunnerPoolChoice = {
  id: "c1111111-1111-4111-8111-111111111111", name: "Shared pool", scope: "organization",
  availability: "available", barriers: ["CC1", "CC2", "CC3"],
};
const TEAM_LAPTOPS: RunnerPoolChoice = {
  id: "c2222222-2222-4222-8222-222222222222", name: "Team laptops", scope: "user",
  availability: "available", barriers: ["CC1"],
};

/** GET /runner-pools, per scenario. */
export const SCENARIO_POOLS: ByScenario<RunnerPoolChoice[]> = everyScenario([SHARED], {
  governed: [{ ...SHARED, barriers: ["CC2", "CC3"] }],
  offline: [{ ...TEAM_LAPTOPS, availability: "unavailable", reason: "runner_pool_no_eligible_member" }],
  noKvm: [{ ...SHARED, barriers: ["CC1", "CC2"] }],
  admin: [SHARED, TEAM_LAPTOPS],
});
