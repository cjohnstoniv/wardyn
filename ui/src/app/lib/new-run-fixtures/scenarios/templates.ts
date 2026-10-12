/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { TemplateList, TemplateSummary } from "../../types/templates";
import { everyScenario, type ByScenario } from "./index";

const MEMBER_PUBLISH = { org: false, groups: [], groups_unverified: false };
const ADMIN_PUBLISH = { org: true, groups: [], groups_unverified: false };

const NIGHTLY: TemplateSummary = {
  id: "d1111111-1111-4111-8111-111111111111", scope: "person", name: "Nightly tests", revision: 2, coverage: "full",
  includes: ["runner", "tools_image"], needs_setup: [], writable: true, updated_at: "2026-10-10T09:00:00Z",
  kind: "template", fields: ["intent.runner_pool_id", "intent.tools"],
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
