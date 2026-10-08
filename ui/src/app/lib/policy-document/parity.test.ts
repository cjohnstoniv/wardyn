/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One policy text must never read as two policies. The CLI's --policy-file
// reader is gopkg.in/yaml.v3 (cmd/wardyn policyToJSON); text the two readers
// would read differently is refused here instead of guessed.
import { describe, expect, it } from "vitest";
import { editPolicySource, parsePolicySource, type PolicySourceFormat } from ".";

const STRAY_BREAK = /\r(?!\n)|[\x85\u2028\u2029]/;

describe("line breaks the readers split differently", () => {
  it.each<[string, PolicySourceFormat, string, number, number, string]>([
    ["a bare CR between keys", "yaml", "a: 1\rb: 2", 1, 5, "Bare carriage return: use LF or CRLF line endings."],
    ["a bare CR in JSON-like YAML", "yaml", '{\r  "a": 1\r}', 1, 2, "Bare carriage return: use LF or CRLF line endings."],
    ["a bare CR after CRLF lines", "yaml", "a: 1\r\nb: 2\r", 2, 5, "Bare carriage return: use LF or CRLF line endings."],
    ["a bare CR as JSON whitespace", "json", '{\r"a":1\r}', 1, 2, "Bare carriage return: use LF or CRLF line endings."],
    ["a bare CR in invalid JSON", "json", '{"a": 1,\r}', 1, 9, "Bare carriage return: use LF or CRLF line endings."],
    ["a plain U+0085", "yaml", "a: x\u0085y", 1, 5, "Unescaped U+0085: write it as \\u0085 in a quoted string."],
    ["a quoted U+0085", "yaml", 'a: "x: y\u0085z"', 1, 9, "Unescaped U+0085: write it as \\u0085 in a quoted string."],
    ["a U+0085 inside a JSON string", "json", '{"a": "x\u0085y"}', 1, 9, "Unescaped U+0085: write it as \\u0085 in a quoted string."],
    ["a U+2028 ending a comment", "yaml", "a: 1 # note\u2028b: 2", 1, 12, "Unescaped U+2028: write it as \\u2028 in a quoted string."],
    ["a U+2029 in a block scalar", "yaml", "a: |\n  x\u2029y\n", 2, 4, "Unescaped U+2029: write it as \\u2029 in a quoted string."],
    ["a U+2028 inside a JSON string", "json", '{"a": "x\u2028y"}', 1, 9, "Unescaped U+2028: write it as \\u2028 in a quoted string."],
  ])("refuses %s at its location", (_, format, source, line, column, message) => {
    const parsed = parsePolicySource(source, format);
    expect(parsed).toEqual({ ok: false, line, column, message });
    expect(parsed).not.toHaveProperty("value");
  });

  it.each<[string, PolicySourceFormat, string, unknown]>([
    ["CRLF YAML with a literal block", "yaml", "a: 1\r\nb: |\r\n  x\r\n  y\r\n", { a: 1, b: "x\ny\n" }],
    ["CRLF JSON", "json", '{\r\n  "a": [\r\n    1\r\n  ]\r\n}\r\n', { a: [1] }],
    ["YAML escapes for the three characters", "yaml", 'a: "x\\u0085y\\Lz\\Pw\\N"', { a: "x\u0085y\u2028z\u2029w\u0085" }],
    ["JSON escapes for the three characters", "json", '{"a": "\\u0085\\u2028\\u2029\\r"}', { a: "\u0085\u2028\u2029\r" }],
  ])("accepts %s", (_, format, source, value) => {
    expect(parsePolicySource(source, format)).toEqual({ ok: true, value });
  });

  it.each<PolicySourceFormat>(["yaml", "json"])("never writes those characters raw from a %s edit", (format) => {
    const text = "x\u0085y\u2028z\u2029w\rv\r\nu";
    const edited = editPolicySource("{}", ["llm_inspection", "classified_markers"], [text], format);
    if (!edited.ok) throw new Error(edited.message);
    expect(edited.source).not.toMatch(STRAY_BREAK);
    expect(parsePolicySource(edited.source, format)).toEqual({ ok: true, value: { llm_inspection: { classified_markers: [text] } } });
  });

  it("refuses an edit of source holding a stray line break", () => {
    expect(editPolicySource("a: 1\rb: 2", ["a"], 2, "yaml")).toMatchObject({ ok: false, line: 1, column: 5 });
  });
});
