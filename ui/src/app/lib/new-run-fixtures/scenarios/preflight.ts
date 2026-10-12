/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ReviewPreflight } from "../../types/new-run-review";
import { everyScenario, type ByScenario } from "./index";

const POOL = { id: "pool-general", name: "General", hosting_type: "remote_provided", revision: 1, selection: "organisation_default" } as const;
const CHECKED: ReviewPreflight = {
  setup_items: [],
  enforced_confinement_class: "CC2",
  runner_pool: POOL,
  runner_pool_slots: { used: 6, max: 10, waiting: 0, verdict: "starts_now", max_wait_sec: 86400 },
  resources_applied: { cpu_millis: { requested: 2000, applied: 2000 }, memory_mib: { requested: 4096, applied: 4096 } },
};
const CEILING = { capped: true, source: "governance", source_name: "Platform team" } as const;

/** POST /runs/preflight's 200 answer, per scenario. */
export const SCENARIO_PREFLIGHT: ByScenario<ReviewPreflight> = everyScenario(CHECKED, {
  governed: {
    ...CHECKED,
    enforced_confinement_class: "CC3",
    resources_applied: {
      cpu_millis: { requested: 16000, applied: 8000, ...CEILING },
      memory_mib: { requested: 16384, applied: 8192, ...CEILING },
    },
  },
  full: {
    ...CHECKED,
    runner_pool: { ...POOL, id: "pool-ci", name: "CI jobs", selection: "explicit" },
    runner_pool_slots: { used: 40, max: 40, waiting: 3, verdict: "waits", max_wait_sec: 86400 },
  },
  noKvm: { ...CHECKED, enforced_confinement_class: "CC1" },
  app: {
    ...CHECKED,
    setup_items: [{ id: "secret:deploy-key", kind: "secret", label: "deploy-key", required_by: "Application", status: "missing" }],
  },
});
