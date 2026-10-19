/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The console's view of the template field registry: shape, closed values and
// parity with the Go constants. Run:
//   cd ui && pnpm vitest run src/app/lib/template-fields.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { RUN_SHAPES } from "./types/runs";
import {
  TEMPLATE_NESTED_CARRIED,
  TEMPLATE_NESTED_EXCLUDED,
  TEMPLATE_POLICY_FIELDS,
  TEMPLATE_REQUEST_FIELDS,
  templateCarriedFields,
  templateFieldRule,
  templateIncludes,
} from "./template-fields";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const goConsts = (src: string, names: string[]) =>
  new Set(names.map((n) => new RegExp(`\\b${n}\\s*=\\s*"([a-z_]+)"`).exec(src)?.[1] ?? `missing ${n}`));

describe("template field registry", () => {
  const root = repoRoot();
  const fields = readFileSync(join(root, "internal/api/template_fields.go"), "utf8");

  it("covers the request and policy fields exactly once each", () => {
    for (const rules of [TEMPLATE_REQUEST_FIELDS, TEMPLATE_POLICY_FIELDS]) {
      expect(new Set(rules.map((r) => r.name)).size).toBe(rules.length);
    }
    expect(TEMPLATE_REQUEST_FIELDS.length).toBeGreaterThanOrEqual(34);
    expect(TEMPLATE_POLICY_FIELDS).toHaveLength(20);
  });

  it("every carried field names its part and omission meaning; every excluded one its reason", () => {
    for (const r of [...TEMPLATE_REQUEST_FIELDS, ...TEMPLATE_POLICY_FIELDS]) {
      if (r.excluded) {
        expect(r.why, r.name).toBeTruthy();
      } else {
        expect(r.omitted, r.name).toBeTruthy();
        expect(r.empty, r.name).toBeTruthy();
        expect(r.meaning, r.name).toBeTruthy();
        if (r.name !== "inline_policy") expect(r.part, r.name).toBeTruthy();
      }
    }
  });

  it("uses the closed values Go declares", () => {
    const omissions = goConsts(fields, ["templateOmitBaseline", "templateOmitRequired", "templateOmitOptional"]);
    const empties = goConsts(fields, ["templateEmptySame", "templateEmptyValue", "templateEmptyAll", "templateEmptyNever"]);
    const exclusions = goConsts(fields, ["templateExcludeSecret", "templateExcludeOrigin", "templateExcludeRetired", "templateExcludeLaunch"]);
    const sections = goConsts(fields, ["templateSectionRunWide", "templateSectionSCMEntry", "templateSectionHarness", "templateSectionComp"]);
    const parts = new Set([...readFileSync(join(root, "pkg/client/templates.go"), "utf8").matchAll(/\bTemplatePart\w+\s+TemplatePart = "([a-z_]+)"/g)].map((m) => m[1]));
    for (const r of [...TEMPLATE_REQUEST_FIELDS, ...TEMPLATE_POLICY_FIELDS]) {
      if (r.omitted) expect(omissions, r.name).toContain(r.omitted);
      if (r.empty) expect(empties, r.name).toContain(r.empty);
      if (r.excluded) expect(exclusions, r.name).toContain(r.excluded);
      if (r.part) expect(parts, r.name).toContain(r.part);
      if (r.section) expect(sections, r.name).toContain(r.section);
    }
  });

  it("excludes the secret-bearing, retired and launch-only fields and nothing else", () => {
    expect(TEMPLATE_REQUEST_FIELDS.filter((r) => r.excluded).map((r) => r.name).sort()).toEqual([
      "description",
      "integration_id",
      "local_folder",
      "max_wait_sec",
      "mode_contract",
      "policy_overlay",
      "preset",
      "preset_version",
      "title",
    ]);
    expect(templateFieldRule("title")?.excluded).toBe("launch");
    expect(TEMPLATE_POLICY_FIELDS.filter((r) => r.excluded)).toEqual([]);
    expect(TEMPLATE_NESTED_EXCLUDED.map((e) => e.path)).toEqual([
      "GrantSpec.owner_only_refs",
      "LLMInspectionSpec.workspace_secret_values",
      "RunLocalFolder.name",
      "RunLocalFolder.remote_dir",
    ]);
    expect(TEMPLATE_NESTED_CARRIED.LLMInspectionSpec).not.toContain("workspace_secret_values");
  });

  it("keeps push rules in the SCM entry, tool rules per harness, and no landed carrier pending", () => {
    expect(templateFieldRule("inline_policy.push_rules")).toMatchObject({ section: "scm_entry", keyed_by: "provider+org" });
    expect(templateFieldRule("inline_policy.tool_rules")).toMatchObject({ section: "harness", keyed_by: "harness" });
    expect(templateFieldRule("model_provider")?.section).toBe("harness");
    expect(templateFieldRule("interactive")?.omitted).toBe("unset_required");
    expect(templateFieldRule("agent")?.omitted).toBe("unset_optional");
    expect(templateFieldRule("runner_pool_id")?.pending).toBeFalsy();
    // The run-mode carriers are real request fields now: no working names, no pending flag.
    for (const name of ["experience", "workload", "tools", "startup", "start_folder", "no_repositories_or_drives"]) {
      expect(templateFieldRule(name), name).toBeDefined();
      expect(templateFieldRule(name)?.pending, name).toBeFalsy();
    }
    expect(templateFieldRule("tools")).toMatchObject({ section: "harness", keyed_by: "harness" });
    expect(templateFieldRule("experience")?.omitted).toBe("unset_required");
    expect(templateFieldRule("workload")?.sensitive).toBe(true);
    for (const name of ["included_tools", "starting_folder"]) expect(templateFieldRule(name), name).toBeUndefined();
    expect(templateFieldRule("pool_id")).toBeUndefined();
    for (const r of [...TEMPLATE_REQUEST_FIELDS, ...TEMPLATE_POLICY_FIELDS]) {
      expect(r.part as string, r.name).not.toBe("info");
    }
  });

  it("carries shape as an optional field, with the three shapes Go declares", () => {
    expect(templateFieldRule("shape")).toMatchObject({ part: "runner", omitted: "unset_optional" });
    expect(templateFieldRule("shape")?.pending).toBeFalsy();
    expect(templateCarriedFields()).toContain("shape");
    const goShapes = new Set([...readFileSync(join(root, "pkg/client/runs_mode.go"), "utf8").matchAll(/\bRunShape\w+\s+RunShape = "([a-z_]+)"/g)].map((m) => m[1]));
    expect(new Set(RUN_SHAPES)).toEqual(goShapes);
  });

  it("looks a field up by request or policy name", () => {
    expect(templateFieldRule("interactive")?.omitted).toBe("unset_required");
    expect(templateFieldRule("inline_policy.allowed_methods")?.empty).toBe("all");
    expect(templateFieldRule("inline_policy.auto_stop_after_sec")?.empty).toBe("never");
    expect(templateFieldRule("runner_id")?.person_only).toBe(true);
    expect(templateFieldRule("task")?.sensitive).toBe(true);
    expect(templateFieldRule("nope")).toBeUndefined();
    expect(templateCarriedFields()).not.toContain("preset");
    expect(templateCarriedFields()).not.toContain("title");
    expect(templateCarriedFields()).toContain("inline_policy.llm_inspection");
  });

  it("names the parts a document includes, counting inline_policy by its fields", () => {
    expect(
      templateIncludes({ agent: "claude-code", drive: { enabled: true }, inline_policy: { allowed_domains: ["a.example.com"], eligible_grants: [] } }),
    ).toEqual(["credentials", "drives", "egress", "tools_image"]);
  });
});
