/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { SetupHarnessTool } from "../../types/setup";
import { everyScenario, type ByScenario } from "./index";

const CLAUDE: SetupHarnessTool = { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true };
const CODEX: SetupHarnessTool = { id: "codex-cli", display: "Codex CLI", has_gateway: true, has_login: true, enabled: true };

/** GET /setup/status `harnesses[]`, per scenario. */
export const SCENARIO_HARNESSES: ByScenario<SetupHarnessTool[]> = everyScenario([CLAUDE, CODEX], {
  member: [CLAUDE, { ...CODEX, providers_ungranted: true }],
});
