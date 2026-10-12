/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { NEW_RUN_REASON, PLACEMENT_REASONS, WORKSPACE_DOOR_REASONS } from "../../../../lib/new-run-refusals";
import { RUN_MODE_REASON } from "../../../../lib/run-mode-refusals";
import { RUNNER_POOL_REASONS } from "../../../../lib/runner-pool-refusals";
import { TEMPLATE_POLICY_FIELDS, TEMPLATE_REQUEST_FIELDS } from "../../../../lib/template-fields";
import { TEMPLATE_REASON } from "../../../../lib/template-refusals";
import { FIELD_HOME, FIELD_HOME_OVERRIDES, HOME_OF_REASON, homeOfPath, homeOfReason, REVIEW_GROUP_OF_FIELD } from "./field-homes";

const REGISTRY = [
  ...TEMPLATE_REQUEST_FIELDS.map((r) => r.name),
  ...TEMPLATE_POLICY_FIELDS.map((r) => `inline_policy.${r.name}`),
].sort();
const sortedKeys = (o: object) => Object.keys(o).sort();

describe("field homes (U7)", () => {
  it("give every registry request field and every policy field exactly one home", () => {
    expect(sortedKeys(FIELD_HOME)).toEqual(REGISTRY);
  });

  it("give every registry field exactly one Review group", () => {
    expect(sortedKeys(REVIEW_GROUP_OF_FIELD)).toEqual(REGISTRY);
  });

  it("never send a policy field to Tools and access, which writes overrides and components only", () => {
    const policyHomes = new Set(TEMPLATE_POLICY_FIELDS.map((r) => FIELD_HOME[`inline_policy.${r.name}`]));
    expect([...policyHomes].sort()).toEqual(["document", "review"]);
    expect(FIELD_HOME.overrides).toBe("tools");
    expect(FIELD_HOME.components).toBe("tools");
  });

  it("keep the launch-only and origin fields out of every step", () => {
    for (const name of ["title", "description", "mode_contract"]) expect(FIELD_HOME[name], name).toBe("excluded");
  });

  it("name subpath homes only under registry fields", () => {
    for (const path of Object.keys(FIELD_HOME_OVERRIDES)) expect(REGISTRY, path).toContain(path.split(/[.[]/)[0]);
  });

  it("give every reason the console names a step, and send an unknown one to Review", () => {
    const known = [
      ...Object.values(NEW_RUN_REASON), ...PLACEMENT_REASONS, ...Object.values(RUN_MODE_REASON),
      ...RUNNER_POOL_REASONS, ...Object.values(TEMPLATE_REASON), ...WORKSPACE_DOOR_REASONS,
    ];
    expect(sortedKeys(HOME_OF_REASON)).toEqual([...new Set(known)].sort());
    expect(homeOfReason("runner_pool_stale")).toBe("run");
    expect(homeOfReason("run_shape_conflict")).toBe("shape");
    expect(homeOfReason("something_new")).toBe("review");
  });

  it("sends the workspace-door refusals to Files, not Review", () => {
    for (const reason of WORKSPACE_DOOR_REASONS) expect(homeOfReason(reason), reason).toBe("files");
  });

  it("resolve a document path to the most specific home", () => {
    expect(homeOfPath("workload.task")).toBe("harnesses");
    expect(homeOfPath("workload.command")).toBe("run");
    expect(homeOfPath("workload")).toBe("run");
    expect(homeOfPath("resources.disk_mib")).toBe("files");
    expect(homeOfPath("resources.cpu_millis")).toBe("review");
    expect(homeOfPath("tools[1].model_provider")).toBe("tools");
    expect(homeOfPath("tools.0.model_provider")).toBe("tools");
    expect(homeOfPath("tools[0].id")).toBe("harnesses");
    expect(homeOfPath("inline_policy.auto_stop_after_sec")).toBe("document");
    expect(homeOfPath("inline_policy.allowed_domains[2]")).toBe("review");
    expect(homeOfPath("inline_policy")).toBe("review");
    expect(homeOfPath("not_a_field")).toBeUndefined();
  });
});
