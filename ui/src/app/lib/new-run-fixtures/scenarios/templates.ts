/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { LaunchIntentList } from "../../types/launch-intents";
import type { TemplateList, TemplateSummary } from "../../types/templates";
import { everyScenario, type ByScenario } from "./index";

const MEMBER_PUBLISH = { org: false, groups: [], groups_unverified: false };
const ADMIN_PUBLISH = { org: true, groups: [], groups_unverified: false };

const NIGHTLY: TemplateSummary = {
  id: "d1111111-1111-4111-8111-111111111111", scope: "person", name: "Nightly tests", revision: 2, coverage: "full",
  includes: ["runner", "tools_image"], needs_setup: [], writable: true, updated_at: "2026-10-10T09:00:00Z",
  kind: "template", fields: ["runner_pool_id", "tools"],
};
const ORG_BASELINE: TemplateSummary = {
  id: "d2222222-2222-4222-8222-222222222222", scope: "org", name: "Organisation baseline", revision: 5, coverage: "partial",
  includes: ["egress", "access_rules"], needs_setup: ["components"], writable: false, updated_at: "2026-10-08T15:30:00Z",
  kind: "policy", fields: ["inline_policy.allowed_domains"],
};

/** GET /templates, per scenario. */
export const SCENARIO_TEMPLATES: ByScenario<TemplateList> = everyScenario<TemplateList>({ templates: [NIGHTLY], publish: MEMBER_PUBLISH }, {
  admin: { templates: [NIGHTLY, { ...ORG_BASELINE, writable: true }], publish: ADMIN_PUBLISH },
  governed: { templates: [NIGHTLY, ORG_BASELINE], publish: MEMBER_PUBLISH },
});

export const NIGHTLY_RUN_ID = "e1111111-1111-4111-8111-111111111111";

/** GET /me/launch-intents, per scenario. Only a run that kept its setup is listed. */
export const SCENARIO_LAUNCH_INTENTS: ByScenario<LaunchIntentList> = everyScenario<LaunchIntentList>({
  intents: [{ run_id: NIGHTLY_RUN_ID, title: "Fix the flaky retry", shape: "agent_task", created_at: "2026-10-11T08:00:00Z" }],
});
