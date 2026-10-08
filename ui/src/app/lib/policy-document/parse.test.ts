/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { parsePolicySource, type PolicySourceMapping } from ".";

const aliasBomb = ["a: &a [x, x, x, x, x, x, x, x, x, x]",
  ...["b", "c", "d", "e"].map((key, index) =>
    `${key}: &${key} [${Array(10).fill(`*${"abcd"[index]}`).join(", ")}]`),
].join("\n");

describe("parsePolicySource refusals", () => {
  it.each([
    ["DUPLICATE_KEY", "a: 1\na: 2", "DUPLICATE_KEY"],
    ["DUPLICATE_KEY nested", "a:\n  b: 1\n  b: 2", "DUPLICATE_KEY"],
    ["DUPLICATE_KEY pasted JSON", '{"a":1,"a":2}', "DUPLICATE_KEY"],
    ["DUPLICATE_KEY escaped JSON key", '{"a":1,"\\u0061":2}', "DUPLICATE_KEY"],
    ["DUPLICATE_KEY nested JSON", '{"a":{"b":1,"b":2}}', "DUPLICATE_KEY"],
    ["MULTIPLE_DOCS", "a: 1\n---\nb: 2", "MULTIPLE_DOCS"],
    ["MULTIPLE_DOCS with empty second document", "a: 1\n---", "MULTIPLE_DOCS"],
    ["MULTIPLE_DOCS JSON values", '{"a":1}\n{"b":2}', ""],
    ["TAB_AS_INDENT", "a:\n\tb: 1", "TAB_AS_INDENT"],
    ["unknown tag warning", "a: !foo bar", "TAG_RESOLVE_FAILED"],
    ["python tag warning", "a: !!python/object:os.system x", "TAG_RESOLVE_FAILED"],
    ["invalid integer tag warning", "a: !!int abc", "TAG_RESOLVE_FAILED"],
    ["explicit non-specific tag", "a: ! x", "tags"],
    ["explicit string tag", "a: !!str 123", "tags"],
    ["explicit integer tag", "a: !!int 12", "tags"],
    ["explicit boolean tag", "a: !!bool true", "tags"],
    ["explicit mapping tag", "a: !!map {b: 1}", "tags"],
    ["explicit sequence tag", "a: !!seq [x]", "tags"],
    ["explicit verbatim tag", "a: !<tag:yaml.org,2002:str> x", "tags"],
    ["explicit root tag", "!!map {a: 1}", "tags"],
    ["explicit key tag", "!!str a: 1", "tags"],
    ["explicit timestamp", "a: !!timestamp 2026-10-07", "tags"],
    ["explicit binary", "a: !!binary aGk=", "tags"],
    ["explicit set", "a: !!set {x: null}", "tags"],
    ["alias", "a: &a x\nb: *a", "Aliases"],
    ["unresolved alias", "a: *missing", "Aliases"],
    ["alias cycle", "a: &a [*a]", "Aliases"],
    ["alias bomb", aliasBomb, "Aliases"],
    ["%YAML 1.1 yes and 017", "%YAML 1.1\n---\na: yes\nb: 017", "Directives"],
    ["%YAML 1.2", "%YAML 1.2\n---\na: 1", "Directives"],
    ["unused %TAG", "%TAG !e! tag:example.com,2026:\n---\na: 1", "Directives"],
    ["redefined core %TAG", "%TAG !! tag:example.com,2026:\n---\na: 1", "Directives"],
    ["unknown directive", "%FUTURE argument\n---\na: 1", "Directives"],
    ["directive after document", "a: 1\n...\n%YAML 1.2", "Directives"],
    ["merge key", "a:\n  <<: {b: 1}", "Merge keys"],
    ["quoted merge key", '"<<": {a: 1}', "Merge keys"],
    ["JSON merge key", '{"a":{"<<":{"b":1}}}', "Merge keys"],
    ["escaped JSON merge key", '{"\\u003c\\u003c":{}}', "Merge keys"],
    ["integer key", "1: a", "keys must be strings"],
    ["boolean key", "true: b", "keys must be strings"],
    ["null key", "~: a", "keys must be strings"],
    ["empty key", ": a", "keys must be strings"],
    ["sequence key", "? [x, y]\n: a", "keys must be strings"],
    ["mapping key", "? {x: y}\n: a", "keys must be strings"],
    ["nested non-string key", "a:\n  1: b", "keys must be strings"],
    ["nonmapping array", "[a, b]", "must be a mapping"],
    ["nonmapping string", "hello", "must be a mapping"],
    ["nonmapping number", "42", "must be a mapping"],
    ["nonmapping null", "null", "must be a mapping"],
    ["empty document", "", "must be a mapping"],
    ["comment-only document", "# comment\n", "must be a mapping"],
    ["NaN", "n: .nan", "Numbers"],
    ["positive infinity", "n: .inf", "Numbers"],
    ["negative infinity", "n: -.Inf", "Numbers"],
    ["exponent overflow", "n: 1e309", "Numbers"],
    ["JSON exponent overflow", '{"n":1e309}', "Numbers"],
    ["unsafe positive integer", "n: 9007199254740992", "Numbers"],
    ["unsafe negative integer", "n: -9007199254740992", "Numbers"],
    ["2^63-1 before Number conversion", "n: 9223372036854775807", "Numbers"],
    ["unsafe pasted JSON integer", '{"n":9223372036854775807}', "Numbers"],
    ["unsafe exponent integer", "n: 1e20", "Numbers"],
    ["unsafe decimal integer", "n: 9007199254740992.0", "Numbers"],
    ["unsafe hexadecimal integer", "n: 0x20000000000000", "Numbers"],
    ["unsafe octal integer", "n: 0o400000000000000000", "Numbers"],
  ])("rejects %s without exposing a value", (_, source, message) => {
    const parsed = parsePolicySource(source);
    expect(parsed.ok).toBe(false);
    if (parsed.ok) throw new Error("refused source returned a value");
    expect(Object.keys(parsed).sort()).toEqual(["column", "line", "message", "ok"]);
    expect(parsed.line).toBeGreaterThanOrEqual(1);
    expect(parsed.column).toBeGreaterThanOrEqual(1);
    expect(parsed.message.length).toBeGreaterThan(0);
    expect(parsed.message).toContain(message);
  });

  it.each([
    ["# heading\na:\n  b: 1\n  b: 2", 4, 3],
    ["# heading\n%YAML 1.1\n---\na: yes", 2, 1],
    ["a: 1\n---\nb: 2", 2, 1],
    ["a:\n  n: .nan", 2, 6],
    ["a:\n  <<: {}", 2, 3],
    ["a: 1\r\nb: *missing", 2, 4],
  ])("locates the failure in %s", (source, line, column) => {
    expect(parsePolicySource(source)).toMatchObject({ ok: false, line, column });
  });

  it("never exposes the last valid result after a broken edit", () => {
    const previous = parsePolicySource("min_confinement_class: CC2");
    expect(previous.ok).toBe(true);
    const current = parsePolicySource("min_confinement_class: [");
    expect(current.ok).toBe(false);
    expect(current).not.toHaveProperty("value");
    expect(current).not.toHaveProperty("document");
  });

  it("catches parser or AST stack exhaustion on deeply nested input", () => {
    const source = `a: ${"[".repeat(20_000)}0${"]".repeat(20_000)}`;
    const parsed = parsePolicySource(source);
    expect(parsed.ok).toBe(false);
    expect(parsed).not.toHaveProperty("value");
  });
});

describe("parsePolicySource JSON-compatible mappings", () => {
  it.each<[string, string, PolicySourceMapping]>([
    ["empty mapping", "{}", {}],
    ["JSON scalars and nested collections", "a: [true, false, null, 1.25, -2, {b: x}]", { a: [true, false, null, 1.25, -2, { b: "x" }] }],
    ["YAML 1.2 core words", "a: yes\nb: off\nc: 17", { a: "yes", b: "off", c: 17 }],
    ["quoted timestamp and binary-looking strings", 'date: "2000-10-07"\nbinary: aGk=', { date: "2000-10-07", binary: "aGk=" }],
    ["quoted unsafe-looking scalars", 'a: ".nan"\nb: ".inf"\nc: "9223372036854775807"\nd: "!!timestamp 2026-10-07"', { a: ".nan", b: ".inf", c: "9223372036854775807", d: "!!timestamp 2026-10-07" }],
    ["quoted string keys", '"1": a\n"true": b\n"null": c\n"": d', { "1": "a", true: "b", null: "c", "": "d" }],
    ["quoted directives and aliases", 'a: "%YAML 1.1"\nb: "%TAG !e! tag:example.com,2026:"\nc: "*missing"\nd: "<<"', { a: "%YAML 1.1", b: "%TAG !e! tag:example.com,2026:", c: "*missing", d: "<<" }],
    ["block text containing forbidden syntax", "a: |\n  %YAML 1.1\n  %TAG !e! tag:example.com,2026:\n  ---\n  !!binary aGk=\n  *missing\n  <<: x\n", { a: "%YAML 1.1\n%TAG !e! tag:example.com,2026:\n---\n!!binary aGk=\n*missing\n<<: x\n" }],
    ["safe integer limits", "min: -9007199254740991\nmax: 9007199254740991", { min: Number.MIN_SAFE_INTEGER, max: Number.MAX_SAFE_INTEGER }],
    ["safe bases and exponents", "hex: 0x10\noctal: 0o17\nexp: 1e3\nsmall: 1e-3", { hex: 16, octal: 15, exp: 1000, small: 0.001 }],
    ["comments and document markers", "# policy\n---\na: 1 # trailing\n...\n# end\n", { a: 1 }],
    ["anchor without aliases", "a: &label x", { a: "x" }],
    ["explicit null versus omitted", "a: null\nb:\n", { a: null, b: null }],
  ])("round-trips %s through the same JSON boundary", (_, source, expected) => {
    const parsed = parsePolicySource(source);
    expect(parsed).toEqual({ ok: true, value: expected });
    if (!parsed.ok) throw new Error(parsed.message);
    expect(Object.keys(parsed).sort()).toEqual(["ok", "value"]);
    const json = JSON.stringify(parsed.value);
    expect(parsePolicySource(json)).toEqual(parsed);
  });

  it("preserves own __proto__ and constructor keys without changing object prototypes", () => {
    const parsed = parsePolicySource('"__proto__": {polluted: true}\nconstructor: x');
    expect(parsed.ok).toBe(true);
    if (!parsed.ok) throw new Error(parsed.message);
    expect(Object.prototype.hasOwnProperty.call(parsed.value, "__proto__")).toBe(true);
    expect(Object.getPrototypeOf(parsed.value)).toBe(Object.prototype);
    expect({}).not.toHaveProperty("polluted");
    expect(JSON.stringify(parsed.value)).toBe('{"__proto__":{"polluted":true},"constructor":"x"}');
  });

  it.each(["", "azure_devops_capabilities: []\n", "azure_devops_capabilities: null\n"])(
    "preserves ADO omitted, empty and null with PAT scope distinctions: %s",
    (ado) => {
      const source = `${ado}eligible_grants:\n  - kind: git_pat\n  - kind: git_pat\n    scope: {}\n  - kind: git_pat\n    scope: {repos: [], api: false, access: null}\n`;
      const parsed = parsePolicySource(source);
      expect(parsed.ok).toBe(true);
      if (!parsed.ok) throw new Error(parsed.message);
      expect(parsed.value.eligible_grants).toEqual([
        { kind: "git_pat" }, { kind: "git_pat", scope: {} },
        { kind: "git_pat", scope: { repos: [], api: false, access: null } },
      ]);
      expect(Object.prototype.hasOwnProperty.call(parsed.value, "azure_devops_capabilities")).toBe(ado !== "");
      if (ado) expect(parsed.value.azure_devops_capabilities).toEqual(ado.includes("[]") ? [] : null);
      expect(parsePolicySource(JSON.stringify(parsed.value))).toEqual(parsed);
    },
  );
});
