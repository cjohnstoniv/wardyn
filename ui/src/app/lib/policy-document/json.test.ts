/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { editPolicySource, parsePolicySource } from ".";

const throwsInJsonParse = (source: string) => {
  try {
    JSON.parse(source);
    return false;
  } catch {
    return true;
  }
};

describe("parsePolicySource in explicit JSON mode", () => {
  it.each([
    ["a trailing comma in an object", '{"a": 1,}', 1, 9, 'Unexpected "}" in JSON.'],
    ["a trailing comma in an array", '{"a": [1,]}', 1, 10, 'Unexpected "]" in JSON.'],
    ["a trailing comma on a later line", '{\r\n  "a": 1,\r\n}', 3, 1, 'Unexpected "}" in JSON.'],
    ["a hash comment", '{"a": 1} # note', 1, 10, 'Unexpected "#" in JSON.'],
    ["a slash comment", '// note\n{"a": 1}', 1, 1, 'Unexpected "/" in JSON.'],
    ["a single-quoted key", "{'a': 1}", 1, 2, `Unexpected "'" in JSON.`],
    ["a single-quoted value", `{"a": 'b'}`, 1, 7, `Unexpected "'" in JSON.`],
    ["an unquoted key", "{a: 1}", 1, 2, 'Unexpected "a" in JSON.'],
    ["an unquoted string value", '{"a": b}', 1, 7, 'Unexpected "b" in JSON.'],
    ["a block mapping", "a: 1", 1, 1, 'Unexpected "a" in JSON.'],
    ["a block sequence under a quoted key", '"a":\n  - 1', 1, 4, 'Unexpected ":" in JSON.'],
    ["a document marker", '---\n{"a": 1}', 1, 1, 'Unexpected "-" in JSON.'],
    ["an anchor", '{"a": &x 1}', 1, 7, 'Unexpected "&" in JSON.'],
    ["a tag", '{"a": !!str b}', 1, 7, 'Unexpected "!" in JSON.'],
    ["a YAML null", '{"a": ~}', 1, 7, 'Unexpected "~" in JSON.'],
    ["a YAML-only escape", '{"a": "\\x41"}', 1, 8, 'Unexpected "\\\\" in JSON.'],
    ["a line break inside a string", '{"a": "x\n  y"}', 1, 9, 'Unexpected "\\n" in JSON.'],
    ["a tab inside a string", '{"a": "x\ty"}', 1, 9, 'Unexpected "\\t" in JSON.'],
    ["a hexadecimal number", '{"a": 0x10}', 1, 8, 'Unexpected "x" in JSON.'],
    ["a leading-zero number", '{"a": 017}', 1, 8, 'Unexpected "1" in JSON.'],
    ["a leading plus", '{"a": +1}', 1, 7, 'Unexpected "+" in JSON.'],
    ["a bare fraction", '{"a": .5}', 1, 7, 'Unexpected "." in JSON.'],
    ["NaN", '{"a": NaN}', 1, 7, 'Unexpected "N" in JSON.'],
    ["a missing comma", '{"a": 1 "b": 2}', 1, 9, 'Unexpected "\\"" in JSON.'],
    ["a byte order mark", '\ufeff{"a": 1}', 1, 1, 'Unexpected "\ufeff" in JSON.'],
    ["a second value", '{"a": 1}\n{"b": 2}', 2, 1, 'Unexpected "{" in JSON.'],
    ["an astral character", '{"a": 😀}', 1, 7, 'Unexpected "😀" in JSON.'],
    ["an error after every valid construct", '{"a": [1.5e-3, -0, "\\u00e9\\/\\n", true, false, null, {}, []] x}', 1, 61, 'Unexpected "x" in JSON.'],
    ["an unterminated string", '{"a": "x', 1, 9, "Unexpected end of JSON."],
    ["an unterminated object", '{\n  "a": 1\n', 3, 1, "Unexpected end of JSON."],
    ["an empty source", "", 1, 1, "Unexpected end of JSON."],
  ])("refuses %s at its location", (_, source, line, column, message) => {
    expect(throwsInJsonParse(source)).toBe(true);
    const parsed = parsePolicySource(source, "json");
    expect(parsed).toEqual({ ok: false, line, column, message });
    expect(parsed).not.toHaveProperty("value");
  });

  it.each([
    ["duplicate keys", '{"a":1,"a":2}', "DUPLICATE_KEY"],
    ["escaped duplicate keys", '{"a":1,"\\u0061":2}', "DUPLICATE_KEY"],
    ["nested duplicate keys", '{"a":{"b":1,"b":2}}', "DUPLICATE_KEY"],
    ["an unsafe integer", '{"n":9223372036854775807}', "Numbers"],
    ["an exponent overflow", '{"n":1e309}', "Numbers"],
    ["a merge key", '{"a":{"<<":{"b":1}}}', "Merge keys"],
    ["an array root", "[1]", "must be a mapping"],
    ["a null root", "null", "must be a mapping"],
  ])("still refuses %s, which JSON.parse accepts", (_, source, message) => {
    expect(throwsInJsonParse(source)).toBe(false);
    const parsed = parsePolicySource(source, "json");
    expect(parsed).toEqual(parsePolicySource(source));
    if (parsed.ok) throw new Error("refused source returned a value");
    expect(parsed.message).toContain(message);
    expect(parsed).not.toHaveProperty("value");
  });

  it.each([
    ["compact", '{"a":{"b":[1,{"c":"x"}]},"d":"e"}'],
    ["two-space", '{\n  "a": {\n    "b": [\n      1,\n      {\n        "c": "x"\n      }\n    ]\n  },\n  "d": "e"\n}\n'],
    ["tab-indented", '{\n\t"a": {\n\t\t"b": [\n\t\t\t1\n\t\t]\n\t}\n}'],
    ["CRLF", '{\r\n  "a": 1,\r\n  "b": []\r\n}\r\n'],
    ["escapes", '{"a": "\\/\\b\\f\\n\\r\\t\\"\\\\\\u00e9\\ud83d\\ude00", "": "x\\u2028y"}'],
    ["numbers", '{"a": 1.0, "b": 1E2, "c": 0.1e1, "d": 1e-7, "e": -2.5, "f": 9007199254740991}'],
    ["empty collections", '{"a": {}, "b": [], "c": [[], {}]}'],
    ["keys YAML would read otherwise", '{"a: b": 1, "c #d": 2, " e ": 3, "true": 4, "1": 5, "~": 6}'],
  ])("accepts %s JSON with the value JSON.parse reads", (_, source) => {
    const parsed = parsePolicySource(source, "json");
    expect(parsed).toEqual({ ok: true, value: JSON.parse(source) });
    expect(parsed).toEqual(parsePolicySource(source));
  });

  // JSON allows a bare carriage return as whitespace; the YAML boundary would
  // read it as content, so this valid JSON is refused before either reading.
  it("refuses valid JSON using a bare carriage return as whitespace", () => {
    const source = '{\r  "a": 1\r}';
    expect(JSON.parse(source)).toEqual({ a: 1 });
    expect(parsePolicySource(source, "json")).toEqual({
      ok: false, line: 1, column: 2, message: "Bare carriage return: use LF or CRLF line endings.",
    });
  });

  it("leaves the default YAML reading of JSON-compatible YAML unchanged", () => {
    for (const source of ['{"a": 1,}', "{a: 1} # note", "{'a': 1}", "a: 1", '{"a": 0x1}']) {
      expect(parsePolicySource(source)).toEqual({ ok: true, value: { a: 1 } });
      expect(parsePolicySource(source, "yaml")).toEqual({ ok: true, value: { a: 1 } });
    }
  });

  it("accepts the JSON its own structured edits produce", () => {
    const edited = editPolicySource("# note\na: [x]\n", ["b", "c"], "line one\nline two", "json");
    if (!edited.ok) throw new Error(edited.message);
    expect(parsePolicySource(edited.source, "json")).toEqual({ ok: true, value: { a: ["x"], b: { c: "line one\nline two" } } });
  });

  // mulberry32 with a fixed seed: one character of valid JSON is replaced,
  // removed or inserted, and JSON.parse stays the judge of what is JSON.
  it("never accepts a mutation JSON.parse refuses, and reads accepted JSON as JSON.parse does", () => {
    let seed = 0x1921;
    const random = () => {
      seed = (seed + 0x6d2b79f5) | 0;
      let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
    const base = JSON.stringify({
      allowed_domains: ["api.example.com", "*.example.org"], min_confinement_class: "CC2", auto_stop_after_sec: 3600,
      eligible_grants: [{ kind: "git_pat", scope: { repos: [], api: false, access: null }, ttl_seconds: 1.5e3 }],
      llm_inspection: { classified_markers: ["line one\nline two", "tab\there", "é😀"] },
    }, null, 2);
    const inserts = [..." \t\n\r,:\"'{}[]#/\\-+.0e1xtn~&*!\u00a0"];
    let refused = 0;
    let accepted = 0;
    for (let index = 0; index < 1500; index++) {
      const at = Math.floor(random() * (base.length + 1));
      const kind = Math.floor(random() * 3);
      const insert = kind === 1 ? "" : inserts[Math.floor(random() * inserts.length)];
      const source = base.slice(0, at) + insert + base.slice(kind === 2 ? at : at + 1);
      const parsed = parsePolicySource(source, "json");
      if (throwsInJsonParse(source)) {
        refused++;
        if (parsed.ok) throw new Error(`accepted invalid JSON: ${JSON.stringify(source)}`);
        expect(parsed.message, JSON.stringify(source)).toMatch(/^(Unexpected (end of JSON|".+" in JSON)|Bare carriage return: use LF or CRLF line endings)\.$/s);
        expect(parsed.line).toBeLessThanOrEqual(source.split("\n").length);
      } else if (parsed.ok) {
        accepted++;
        expect(JSON.stringify(parsed.value)).toBe(JSON.stringify(JSON.parse(source)));
      }
    }
    expect(refused).toBeGreaterThan(500);
    expect(accepted).toBeGreaterThan(500);
  });
});
