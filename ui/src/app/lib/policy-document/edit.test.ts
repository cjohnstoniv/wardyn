/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { editPolicySource, parsePolicySource, type PolicySourceEditResult, type PolicySourceValue } from ".";

function edited(result: PolicySourceEditResult): string {
  if (!result.ok) throw new Error(result.message);
  expect(Object.keys(result).sort()).toEqual(["ok", "source"]);
  return result.source;
}

const source = `# authored policy
min_confinement_class: CC2 # floor stays
eligible_grants:
  - kind: git_pat
    scope:
      host: github.com # host stays
      repos: [acme/one] # repo scope
  # separate grant stays
  - kind: api_key
    scope: {host: api.example.com}
# capability defaults
azure_devops_capabilities: []
`;

describe("editPolicySource", () => {
  it("edits one field while preserving unrelated YAML comments and values", () => {
    const next = edited(editPolicySource(source, ["eligible_grants", 0, "scope", "repos"], ["acme/two"], "yaml"));
    for (const comment of ["# authored policy", "# floor stays", "# host stays", "# separate grant stays", "# capability defaults"]) {
      expect(next).toContain(comment);
    }
    const before = parsePolicySource(source);
    const after = parsePolicySource(next);
    expect(before.ok && after.ok).toBe(true);
    if (!before.ok || !after.ok) throw new Error("valid edit did not parse");
    expect(after.value).toEqual({ ...before.value, eligible_grants: [
      { kind: "git_pat", scope: { host: "github.com", repos: ["acme/two"] } },
      { kind: "api_key", scope: { host: "api.example.com" } },
    ] });
  });

  it("retains an edited scalar's trailing comment", () => {
    const next = edited(editPolicySource(source, ["min_confinement_class"], "CC3", "yaml"));
    expect(next).toContain("min_confinement_class: CC3 # floor stays");
  });

  it.each(["yaml", "json"] as const)("keeps PAT empty, null and omitted distinct in %s", (format) => {
    const path = ["eligible_grants", 0, "scope", "repos"];
    let current = source;
    for (const value of [[], null, undefined]) {
      current = edited(editPolicySource(current, path, value, format));
      const parsed = parsePolicySource(current);
      expect(parsed.ok).toBe(true);
      if (!parsed.ok) throw new Error(parsed.message);
      const grants = parsed.value.eligible_grants as { scope: Record<string, PolicySourceValue> }[];
      expect(Object.prototype.hasOwnProperty.call(grants[0].scope, "repos")).toBe(value !== undefined);
      if (value !== undefined) expect(grants[0].scope.repos).toEqual(value);
      expect(grants[0].scope.host).toBe("github.com");
      expect(parsed.value.azure_devops_capabilities).toEqual([]);
    }
  });

  it("removes and restores ADO defaults without inventing an empty selection", () => {
    const omitted = edited(editPolicySource(source, ["azure_devops_capabilities"], undefined, "yaml"));
    expect(parsePolicySource(omitted)).not.toHaveProperty("value.azure_devops_capabilities");
    const empty = edited(editPolicySource(omitted, ["azure_devops_capabilities"], [], "yaml"));
    expect(parsePolicySource(empty)).toHaveProperty("value.azure_devops_capabilities", []);
  });

  it("creates nested scope fields without aliases for shared JSON objects", () => {
    const scope = { repos: [] };
    const next = edited(editPolicySource("{}", ["eligible_grants"], [{ scope }, { scope }], "yaml"));
    expect(next).not.toMatch(/[&*]a\d/);
    expect(parsePolicySource(next)).toEqual({ ok: true, value: { eligible_grants: [{ scope }, { scope }] } });
    const nested = edited(editPolicySource("{}", ["eligible_grants", 0, "scope", "repos"], [], "yaml"));
    expect(parsePolicySource(nested)).toEqual({ ok: true, value: { eligible_grants: [{ scope: { repos: [] } }] } });
  });

  it("preserves string values that resemble YAML syntax", () => {
    const value = [".inf", "9223372036854775807", "%TAG !e! tag:example.com,2026:", "*missing", "yes"];
    const next = edited(editPolicySource("{}", ["values"], value, "yaml"));
    expect(parsePolicySource(next)).toEqual({ ok: true, value: { values: value } });
  });

  it("keeps JSON edits as JSON and drops comments only from the converted result", () => {
    const json = edited(editPolicySource(source, ["min_confinement_class"], "CC3", "json"));
    expect(() => JSON.parse(json) as unknown).not.toThrow();
    expect(json).not.toContain("#");
    expect(source).toContain("# authored policy");
    const second = edited(editPolicySource(json, ["new"], "yes", "json"));
    expect(JSON.parse(second)).toHaveProperty("new", "yes");
    expect(parsePolicySource(second)).toHaveProperty("value.min_confinement_class", "CC3");
  });

  it("emits comment-free JSON values for save/launch without mutating authored text", () => {
    const parsed = parsePolicySource(source);
    if (!parsed.ok) throw new Error(parsed.message);
    const wire = JSON.stringify(parsed.value);
    expect(wire).not.toContain("#");
    expect(parsePolicySource(wire)).toEqual(parsed);
    expect(source).toContain("# authored policy");
  });

  it("deletes a sequence item and appends at its end", () => {
    const deleted = edited(editPolicySource("a: [one, two]", ["a", 0], undefined, "yaml"));
    expect(parsePolicySource(deleted)).toHaveProperty("value.a", ["two"]);
    const appended = edited(editPolicySource(deleted, ["a", 1], "three", "yaml"));
    expect(parsePolicySource(appended)).toHaveProperty("value.a", ["two", "three"]);
  });

  it("keeps the exact source when deleting an absent field", () => {
    expect(editPolicySource(source, ["absent"], undefined, "yaml")).toEqual({ ok: true, source });
  });

  it.each(["a: [", "a: 1\na: 2", '{"a":1,"a":2}', "a: *missing"])(
    "refuses structured edits of invalid source without producing replacement text: %s", (invalid) => {
      const previous = edited(editPolicySource(source, ["min_confinement_class"], "CC3", "yaml"));
      expect(previous).toContain("CC3");
      const current = editPolicySource(invalid, ["min_confinement_class"], "CC2", "yaml");
      expect(current).toEqual(parsePolicySource(invalid));
      expect(current).not.toHaveProperty("source");
      expect(current).not.toHaveProperty("value");
    },
  );

  it.each([[], ["<<"], ["a", "<<"], ["a", 2], ["a", -1], ["a", 0.5], ["a", "0"], ["b", "child"], [0], ["missing", 2]]
    .map((path) => ({ path, label: JSON.stringify(path) })))(
    "refuses invalid edit path $label", ({ path }) => {
      const result = editPolicySource("a: [x]\nb: scalar", path, "changed", "yaml");
      expect(result.ok).toBe(false);
      expect(result).not.toHaveProperty("source");
    },
  );

  it.each<[string, unknown]>([
    ["NaN", NaN], ["Infinity", Infinity], ["unsafe integer", Number.MAX_SAFE_INTEGER + 1],
    ["nested Infinity", { n: -Infinity }], ["merge key", { "<<": {} }],
    ["Date", new Date("2026-10-07")], ["binary", new Uint8Array([1])], ["Map", new Map([["a", 1]])],
    ["BigInt", 1n], ["undefined field", { missing: undefined }], ["sparse array", Array(1)],
  ])("refuses non-JSON or unsafe edit value %s before serialization", (_, value) => {
    const result = editPolicySource("{}", ["value"], value as PolicySourceValue, "json");
    expect(result.ok).toBe(false);
    expect(result).not.toHaveProperty("source");
  });

  it("catches cycles in edit values", () => {
    const cycle: PolicySourceValue[] = [];
    cycle.push(cycle);
    expect(editPolicySource("{}", ["value"], cycle, "yaml").ok).toBe(false);
  });
});
