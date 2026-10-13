/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { TEMPLATE_REFUSAL } from "../../../../lib/template-refusals";
import type { TemplatePublish } from "../../../../lib/types/templates";
import type { RunIntent } from "../draft/run-intent";
import {
  buildSaveRequest, leaveOutOwnDrives, missingDependencies, ONLY_ME, pickIntent, POLICY_KEY_PART, saveAudiences, savePartsOf,
} from "./save-as-dialog.model";

const publish = (p: Partial<TemplatePublish>): TemplatePublish => ({ org: false, groups: [], groups_unverified: false, ...p });

describe("the policy keys a policy may hold", () => {
  it("are the run-wide network, credential and access-rule keys the server pins", () => {
    // internal/api TestTemplatePolicyKeysArePinned lists the same nine.
    expect(Object.keys(POLICY_KEY_PART).sort()).toEqual([
      "allow_all_egress", "allowed_domains", "allowed_methods", "denied_domains", "eligible_grants",
      "first_use_approval", "first_use_hold_seconds", "llm_inspection", "max_holds",
    ]);
  });
});

describe("saveAudiences", () => {
  it("offers Only me and the managed groups, never the organisation", () => {
    const got = saveAudiences(publish({ org: true, groups: ["g-1", "g-2"], group_labels: { "g-1": "Platform" } }));
    expect(got.map((a) => [a.value, a.scope, a.groupId, a.label])).toEqual([
      [ONLY_ME, "person", undefined, undefined],
      ["group:g-1", "group", "g-1", "Platform"],
      ["group:g-2", "group", "g-2", undefined],
    ]);
  });

  it("offers no group while the sign-in's groups are unverified", () => {
    expect(saveAudiences(publish({ groups: ["g-1"], groups_unverified: true })).map((a) => a.value)).toEqual([ONLY_ME]);
  });
});

describe("savePartsOf", () => {
  const intent = {
    experience: "background", runner_pool_id: "p1", workspaces: [{ workspace_id: "w1" }], no_end: false,
    inline_policy: { allowed_domains: ["pypi.org"], eligible_grants: [], tool_rules: [] },
  } as unknown as RunIntent;

  it("splits a template by Review group, keeps explicit empties, and files overlay keys under their group", () => {
    const { parts, unassigned } = savePartsOf("template", intent);
    expect(parts.map((p) => [p.id, p.fields])).toEqual([
      ["run", ["experience"]],
      ["where", ["runner_pool_id"]],
      ["work", ["workspaces"]],
      ["access", ["inline_policy.allowed_domains", "inline_policy.eligible_grants", "inline_policy.tool_rules"]],
      ["limits", ["no_end"]],
    ]);
    expect(unassigned).toEqual([]);
  });

  it("holds a policy to its three parts and drops every other key", () => {
    const { parts, unassigned } = savePartsOf("policy", intent);
    expect(parts.map((p) => [p.id, p.fields])).toEqual([
      ["network", ["inline_policy.allowed_domains"]],
      ["credentials", ["inline_policy.eligible_grants"]],
    ]);
    expect(unassigned).toEqual([]);
  });

  it("finds nothing to save in a draft with no policy keys", () => {
    expect(savePartsOf("policy", { experience: "background" } as RunIntent).parts).toEqual([]);
  });

  it("never carries a run title, description or contract marker", () => {
    const stray = { title: "Nightly", description: "d", mode_contract: "0.9", experience: "background" } as unknown as RunIntent;
    const { parts, unassigned } = savePartsOf("template", stray);
    expect([...parts.flatMap((p) => p.fields), ...unassigned]).toEqual(["experience"]);
  });
});

describe("pickIntent", () => {
  it("keeps present empties and rebuilds the overlay from the chosen keys only", () => {
    const intent = { no_end: false, runner_pool_id: "p1", inline_policy: { allowed_domains: [], denied_domains: ["x"] } } as unknown as RunIntent;
    expect(pickIntent(intent, ["no_end", "inline_policy.allowed_domains"])).toEqual({ no_end: false, inline_policy: { allowed_domains: [] } });
  });
});

describe("leaveOutOwnDrives", () => {
  const intent = { drives: [{ drive_id: "mine" }, { drive_id: "team" }], experience: "background" } as unknown as RunIntent;

  it("removes the saver's own drives and says it did", () => {
    const got = leaveOutOwnDrives(intent, new Set(["mine"]));
    expect(got.leftOut).toBe(true);
    expect((got.intent as { drives: unknown }).drives).toEqual([{ drive_id: "team" }]);
  });

  it("drops the key when every drive was the saver's own", () => {
    const got = leaveOutOwnDrives({ drives: [{ drive_id: "mine" }] } as unknown as RunIntent, new Set(["mine"]));
    expect(got).toEqual({ intent: {}, leftOut: true });
  });

  it("leaves a draft with no own drive untouched", () => {
    expect(leaveOutOwnDrives(intent, new Set(["other"]))).toEqual({ intent, leftOut: false });
  });
});

describe("buildSaveRequest", () => {
  it("names a group by its id and lists needs_setup only when there are some", () => {
    const req = buildSaveRequest({
      kind: "template", audience: { value: "group:g", scope: "group", groupId: "g" }, name: "  Nightly ",
      intent: { experience: "background" } as RunIntent, coverage: "partial", needsSetup: [{ field: "agent" }],
    });
    expect(req).toEqual({
      scope: "group", group_id: "g", name: "Nightly", kind: "template",
      document: { api_version: "wardyn/v1", kind: "RunTemplate", coverage: "partial", intent: { experience: "background" }, needs_setup: [{ field: "agent" }] },
    });
    const plain = buildSaveRequest({ kind: "policy", audience: { value: ONLY_ME, scope: "person" }, name: "p", intent: {}, coverage: "partial" });
    expect(plain.group_id).toBeUndefined();
    expect(plain.document.needs_setup).toBeUndefined();
  });
});

describe("missingDependencies", () => {
  it("reads the needed field out of the shared sentence, once each, and ignores other reasons", () => {
    const dep = (path: string, needs: string) => ({ path, reason: "template_dependency_missing", message: TEMPLATE_REFUSAL.DEPENDENCY_MISSING(path, needs) });
    const got = missingDependencies([
      dep("intent.model_provider", "agent"), dep("intent.tool_approvals", "agent"), dep("intent.workload", "experience"),
      { path: "intent", reason: "template_field_invalid", message: TEMPLATE_REFUSAL.DEPENDENCY_MISSING("intent.x", "y") },
    ]);
    expect(got).toEqual([{ field: "agent" }, { field: "experience" }]);
  });
});
