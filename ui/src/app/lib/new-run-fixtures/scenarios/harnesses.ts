/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { SetupHarnessTool } from "../../types/setup";
import { everyScenario, type ByScenario } from "./index";

const CLAUDE: SetupHarnessTool = {
  id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, installed: true, approval_hold: true,
};
const CODEX: SetupHarnessTool = {
  id: "codex-cli", display: "Codex CLI", has_gateway: true, has_login: true, enabled: true, installed: true, approval_hold: false,
};
/** "Your own tools" is installed whenever the base image resolves, so a stock install lists it enabled. */
const OWN_TOOLS: SetupHarnessTool = {
  id: "none", display: "Your own tools", has_gateway: false, has_login: false, no_managed_auth: true, enabled: true, installed: true, approval_hold: false,
};

/** GET /setup/status `harnesses[]`, per scenario. */
export const SCENARIO_HARNESSES: ByScenario<SetupHarnessTool[]> = everyScenario([CLAUDE, CODEX, OWN_TOOLS], {
  member: [CLAUDE, { ...CODEX, providers_ungranted: true }, OWN_TOOLS],
});

/** GET /setup/status `harness_sets`, per scenario: the combinations one declared image carries. */
export const SCENARIO_HARNESS_SETS: ByScenario<string[][]> = everyScenario([["claude-code", "codex-cli"]]);
