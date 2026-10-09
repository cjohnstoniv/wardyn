/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunPolicySpec } from "../../../lib/types";
import { toYaml } from "../yaml-block";
import { applySpecChange, parseSpec, setSpecKey, specToSource } from "./policy-source";

const SOURCE = `# who may reach what
min_confinement_class: CC2 # the floor
allowed_domains:
  - api.example.com # model host
first_use_approval: deny_with_review
`;

const spec = (text: string) => {
  const parsed = parseSpec(text);
  if (!parsed.ok) throw new Error(parsed.message);
  return parsed.spec;
};

describe("parseSpec", () => {
  it("reads YAML by default, and JSON text as the YAML it also is", () => {
    expect(spec(SOURCE)).toEqual({
      min_confinement_class: "CC2",
      allowed_domains: ["api.example.com"],
      first_use_approval: "deny_with_review",
    });
    expect(spec('{"allowed_domains": []}')).toEqual({ allowed_domains: [] });
  });

  it("explicit JSON refuses YAML text, with a place and a reason and no value", () => {
    const parsed = parseSpec(SOURCE, "json");
    expect(parsed).toEqual({ ok: false, line: 1, column: 1, message: expect.any(String) });
    expect("spec" in parsed).toBe(false);
  });

  it("a broken source has a location, and never a value", () => {
    const parsed = parseSpec("a: [");
    expect(parsed.ok).toBe(false);
    expect(parsed).toMatchObject({ line: expect.any(Number), column: expect.any(Number), message: expect.any(String) });
    expect("spec" in parsed).toBe(false);
  });
});

describe("specToSource", () => {
  it("writes YAML through the one display emitter, and JSON as pretty JSON", () => {
    const value = spec(SOURCE);
    expect(specToSource(value)).toBe(`${toYaml(value)}\n`);
    expect(specToSource(value, "json")).toBe(JSON.stringify(value, null, 2));
    expect(spec(specToSource(value))).toEqual(value);
  });
});

describe("structured edits", () => {
  it("rewrite only the key that changed, keeping every unrelated line and comment", () => {
    const current = spec(SOURCE);
    const edited = applySpecChange(SOURCE, "yaml", current, {
      ...current,
      tool_rules: [{ tool: "Bash", effect: "hold" }],
    } as RunPolicySpec);
    if (!edited.ok) throw new Error(edited.message);
    expect(edited.source).toContain("# who may reach what");
    expect(edited.source).toContain("min_confinement_class: CC2 # the floor");
    expect(edited.source).toContain("- api.example.com # model host");
    expect(spec(edited.source).tool_rules).toEqual([{ tool: "Bash", effect: "hold" }]);
  });

  it("removing a key the control dropped leaves the rest untouched", () => {
    const current = spec(SOURCE);
    const { first_use_approval: _dropped, ...rest } = current;
    const edited = applySpecChange(SOURCE, "yaml", current, rest as RunPolicySpec);
    if (!edited.ok) throw new Error(edited.message);
    expect(edited.source).not.toContain("first_use_approval");
    expect(edited.source).toContain("- api.example.com # model host");
  });

  it("no change is no rewrite: the source comes back byte for byte", () => {
    const current = spec(SOURCE);
    expect(applySpecChange(SOURCE, "yaml", current, { ...current })).toEqual({ ok: true, source: SOURCE });
  });

  it("a JSON source stays JSON", () => {
    const json = JSON.stringify(spec(SOURCE), null, 2);
    const edited = setSpecKey(json, "json", "allow_all_egress", true);
    if (!edited.ok) throw new Error(edited.message);
    expect(JSON.parse(edited.source)).toEqual({ ...spec(SOURCE), allow_all_egress: true });
  });

  it("a refused edit returns the refusal and no source", () => {
    const refused = setSpecKey(SOURCE, "yaml", "auto_stop_after_sec", Infinity);
    expect(refused).toEqual({
      ok: false,
      line: 1,
      column: 1,
      message: "The edit must contain only JSON-compatible values and safe numbers.",
    });
    expect(setSpecKey("a: [", "yaml", "x", 1).ok).toBe(false);
  });
});
