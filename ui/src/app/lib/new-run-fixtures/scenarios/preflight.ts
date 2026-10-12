/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { PreflightResult } from "../../types/preflight";
import { everyScenario, type ByScenario } from "./index";

const CHECKED: PreflightResult = { setup_items: [], enforced_confinement_class: "CC2" };

/** POST /runs/preflight's 200 answer, per scenario. */
export const SCENARIO_PREFLIGHT: ByScenario<PreflightResult> = everyScenario(CHECKED, {
  governed: { ...CHECKED, enforced_confinement_class: "CC3" },
  noKvm: { ...CHECKED, enforced_confinement_class: "CC1" },
  app: {
    ...CHECKED,
    setup_items: [{ id: "secret:deploy-key", kind: "secret", label: "deploy-key", required_by: "Application", status: "missing" }],
  },
});
