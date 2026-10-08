/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One policy text must never read as two policies. The CLI's --policy-file
// reader is gopkg.in/yaml.v3 (cmd/wardyn policyToJSON); text the two readers
// would read differently is refused here instead of guessed.
import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { toYaml } from "../../components/wardyn/code-block";
import { editPolicySource, parsePolicySource, type PolicySourceFormat, type PolicySourceMapping } from ".";

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

describe("nesting and size are checked before anything recurses", () => {
  const nested = (levels: number) => '{"a":' + "[".repeat(levels - 1) + "]".repeat(levels - 1) + "}";
  const TOO_DEEP = "Policy source is nested too deeply.";

  // A stack overflow while composing used to leave V8 unable to compile the
  // next regular expression: the second parse aborted the whole process.
  it.each<PolicySourceFormat>(["yaml", "json"])("refuses 1000-deep nesting on consecutive %s parses", (format) => {
    // Every parse runs before any assertion, as an editor reparsing per keystroke would.
    const results = [nested(1000), nested(1000), "[".repeat(100_000), nested(1000)].map((source) => parsePolicySource(source, format));
    for (const parsed of results) {
      expect(parsed).toMatchObject({ ok: false, message: TOO_DEEP });
      expect(parsed).not.toHaveProperty("value");
    }
  });

  it.each<PolicySourceFormat>(["yaml", "json"])("accepts 64 levels and refuses the 65th at its bracket in %s mode", (format) => {
    expect(parsePolicySource(nested(64), format)).toMatchObject({ ok: true });
    expect(parsePolicySource(nested(65), format)).toEqual({ ok: false, line: 1, column: 69, message: TOO_DEEP });
  });

  it.each([
    ["nested block sequences on one line", "a:\n  " + "- ".repeat(100) + "x\n", 2, 129],
    ["indented block mappings", Array.from({ length: 100 }, (_, i) => " ".repeat(i) + "k:").join("\n") + " x\n", 65, 65],
    ["an unclosed JSON array", "[".repeat(200_000), 1, 65],
  ])("refuses %s", (_, source, line, column) => {
    expect(parsePolicySource(source)).toEqual({ ok: false, line, column, message: TOO_DEEP });
  });

  it("refuses invalid JSON nested past the limit at its bracket", () => {
    expect(parsePolicySource("[".repeat(200_000), "json")).toEqual({ ok: false, line: 1, column: 65, message: TOO_DEEP });
  });

  it("refuses an edit of source nested past the limit", () => {
    expect(editPolicySource(nested(65), ["b"], 1, "yaml")).toMatchObject({ ok: false, message: TOO_DEEP });
  });

  // The server refuses a request body over 1 MiB, so no larger policy can launch.
  it.each<PolicySourceFormat>(["yaml", "json"])("refuses %s source over 1 MiB before reading it", (format) => {
    const fits = format === "json" ? `{"a":"${"x".repeat((1 << 20) - 8)}"}` : `a: ${"x".repeat((1 << 20) - 3)}`;
    expect(fits.length).toBe(1 << 20);
    expect(parsePolicySource(fits, format)).toMatchObject({ ok: true });
    expect(parsePolicySource(`${fits} `, format)).toEqual({ ok: false, line: 1, column: 1, message: "Policy source is too large." });
  });
});

describe("plain scalars the CLI's reader resolves differently", () => {
  const LEADING_ZERO = "Leading zeros are ambiguous: remove them, or quote the value.";
  const QUOTE_IT = "Ambiguous unquoted value: quote it.";

  it.each([
    ["a leading-zero integer", "auto_stop_after_sec: 017", 1, 22, LEADING_ZERO],
    ["a signed leading-zero integer", "a:\n  n: -017", 2, 6, LEADING_ZERO],
    ["a leading zero before an 8", "ttl_seconds: 08", 1, 14, LEADING_ZERO],
    ["a zero-padded zero", "n: 00", 1, 4, LEADING_ZERO],
    ["a leading-zero decimal fraction", "n: 01.5", 1, 4, LEADING_ZERO],
    ["a leading-zero flow item", "a: [1, 017]", 1, 8, LEADING_ZERO],
    ["an unquoted date", "tool_rules:\n  - pattern: 2000-10-07", 2, 14, QUOTE_IT],
    ["an unquoted date-time", "a: 2001-12-14t21:59:43.10-05:00", 1, 4, QUOTE_IT],
    ["a space-separated timestamp", "a: 2001-12-14 21:59:43.10 -5", 1, 4, QUOTE_IT],
    ["an unquoted date key", "2000-10-07: x", 1, 1, QUOTE_IT],
    ["underscored digits", "auto_stop_after_sec: 3_600", 1, 22, QUOTE_IT],
    ["a trailing underscore", "n: 1_", 1, 4, QUOTE_IT],
    ["a binary literal", "n: 0b101", 1, 4, QUOTE_IT],
    ["an uppercase hex prefix", "n: 0X1F", 1, 4, QUOTE_IT],
    ["a signed hex literal", "n: -0x10", 1, 4, QUOTE_IT],
    ["a signed octal literal", "n: +0o17", 1, 4, QUOTE_IT],
    ["an underscored hex literal", "n: 0x_1F", 1, 4, QUOTE_IT],
  ])("refuses %s at its location", (_, source, line, column, message) => {
    const parsed = parsePolicySource(source);
    expect(parsed).toEqual({ ok: false, line, column, message });
    expect(parsed).not.toHaveProperty("value");
  });

  it.each<[string, string, unknown]>([
    ["zero, signed zero and plain decimals", "a: 0\nb: -0\nc: 3600\nd: 0.5\ne: -1.25", { a: 0, b: 0, c: 3600, d: 0.5, e: -1.25 }],
    ["lower-case hex, 0o octal and exponents", "a: 0x1F\nb: 0o17\nc: 1e3\nd: 1.5E-3", { a: 31, b: 15, c: 1000, d: 0.0015 }],
    ["quoted look-alikes", "a: \"017\"\nb: '2000-10-07'\nc: \"1_000\"\nd: \"0b1\"", { a: "017", b: "2000-10-07", c: "1_000", d: "0b1" }],
    ["a date in a block scalar", "a: |\n  2000-10-07\n", { a: "2000-10-07\n" }],
    ["strings that only start like numbers", "a:\n  - 1.2.3\n  - 10.0.0.1\n  - 1password.com\n  - 30s\n  - 2024-q3\n  - _1\n  - 1e3x\n",
      { a: ["1.2.3", "10.0.0.1", "1password.com", "30s", "2024-q3", "_1", "1e3x"] }],
    ["YAML 1.1 words, strings in both readers", "a: yes\nb: off\nc: on\nd: y", { a: "yes", b: "off", c: "on", d: "y" }],
  ])("accepts %s", (_, source, value) => {
    expect(parsePolicySource(source)).toEqual({ ok: true, value });
  });

  it("quotes ambiguous strings and keys that a structured edit writes", () => {
    const values = ["017", "1_000", "0b1", "0X1F", "-0x10", "2000-10-07", "2001-12-14 21:59:43.10 -5"];
    const edited = editPolicySource("a: plain\n", ["a"], "2000-10-07", "yaml");
    if (!edited.ok) throw new Error(edited.message);
    expect(edited.source).toBe('a: "2000-10-07"\n');
    const list = editPolicySource("{}", ["values"], values, "yaml");
    if (!list.ok) throw new Error(list.message);
    expect(parsePolicySource(list.source)).toEqual({ ok: true, value: { values } });
    const keys = editPolicySource("{}", ["map"], Object.fromEntries(values.map((key, index) => [key, index])), "yaml");
    if (!keys.ok) throw new Error(keys.message);
    expect(parsePolicySource(keys.source)).toEqual({ ok: true, value: { map: Object.fromEntries(values.map((key, index) => [key, index])) } });
  });
});

// cmd/wardyn's TestPolicyToJSON_ReadsWhatTheConsoleReads reads the same file
// and asserts the CLI produces this value for every snippet accepted here.
type ParityCase = {
  name: string;
  source: string;
  format?: PolicySourceFormat;
  emitted?: boolean;
  console: { value: PolicySourceMapping } | { error: string };
};
const parityCases = JSON.parse(
  fs.readFileSync(path.resolve(__dirname, "../../../../../cmd/wardyn/testdata/policy-reader-parity.json"), "utf8"),
) as ParityCase[];

describe("the cross-reader fixture shared with the CLI's tests", () => {
  it("holds accepted and refused snippets", () => {
    expect(parityCases.filter((c) => "value" in c.console).length).toBeGreaterThanOrEqual(8);
    expect(parityCases.filter((c) => "error" in c.console).length).toBeGreaterThanOrEqual(5);
  });

  it.each(parityCases.map((c) => [c.name, c] as const))("reads %s as the fixture says", (_, c) => {
    const parsed = parsePolicySource(c.source, c.format);
    if ("value" in c.console) expect(parsed).toEqual({ ok: true, value: c.console.value });
    else expect(parsed).toMatchObject({ ok: false, message: c.console.error });
    if (c.emitted && "value" in c.console) expect(toYaml(c.console.value)).toBe(c.source);
  });
});

// YAML 1.2 (yaml here) ends a plain key at a ':' followed by ',', ']' or '}';
// gopkg.in/yaml.v3 keeps that ':' in the scalar, so [x:] is [{x: null}] here
// and ["x:"] there. Found by the console-versus-CLI differential.
describe("a ':' directly before a flow indicator", () => {
  const COLON = "Ambiguous ':' after an unquoted key: add a space after it, or quote the key.";

  it.each([
    ["a flow sequence item", "allowed_domains: [api.example.com:]", 1, 34],
    ["a flow mapping key", "a: {x:, y: 1}", 1, 6],
    ["a key spaced from its colon", "a: [x :]", 1, 7],
    ["a nested flow item", "a: [[x:]]", 1, 7],
    ["an explicit flow key", "a: [? x:]", 1, 8],
    ["an inner item only", "a: [x: [y:]]", 1, 10],
  ])("refuses %s at the ':'", (_, source, line, column) => {
    expect(parsePolicySource(source)).toEqual({ ok: false, line, column, message: COLON });
  });

  it.each<[string, string, unknown]>([
    ["a space after the ':'", "a: [x: ]", { a: [{ x: null }] }],
    ["a value after the ':'", "a: [x: 1]", { a: [{ x: 1 }] }],
    ["a ':' inside a plain scalar", "a: [x:1]", { a: ["x:1"] }],
    ["a quoted key", 'a: ["x":, {"y":}]', { a: [{ x: null }, { y: null }] }],
    ["a line break after the ':'", "a: [x:\n]", { a: [{ x: null }] }],
  ])("accepts %s", (_, source, value) => {
    expect(parsePolicySource(source)).toEqual({ ok: true, value });
  });

  it("never writes the shape from a structured edit", () => {
    const edited = editPolicySource("a: [x, {y: 1}]\n", ["a", 2], { k: null }, "yaml");
    if (!edited.ok) throw new Error(edited.message);
    expect(parsePolicySource(edited.source)).toEqual({ ok: true, value: { a: ["x", { y: 1 }, { k: null }] } });
  });
});

// gopkg.in/yaml.v3 ends an anchor name at ':' or '?' and reads the rest as the
// next scalar: `&x:y a: 1` is {"a": 1} here and {":y a": 1} there. Aliases are
// already refused, so an anchor can never be used; it is refused outright.
describe("anchors", () => {
  const ANCHOR = "Anchors are not allowed.";

  it.each([
    ["an anchor name with ':' on a key", "&x:y a: 1", 1, 1],
    ["an anchor name with ':' on a value", "a: &x:y 1", 1, 4],
    ["an anchor name with '?'", "&x?y a: 1", 1, 1],
    ["an anchored root mapping", "&x:y {}", 1, 1],
    ["a plain anchor", "a: &label x", 1, 4],
    ["an anchor in a flow sequence", "a: [&x b]", 1, 5],
    ["an anchored block mapping", "a: &m\n  b: 1", 1, 4],
  ])("refuses %s at the '&'", (_, source, line, column) => {
    expect(parsePolicySource(source)).toEqual({ ok: false, line, column, message: ANCHOR });
  });

  it("accepts '&' inside scalars", () => {
    expect(parsePolicySource('a: x&y\nb: "&z"\nc: \'&w\'')).toEqual({ ok: true, value: { a: "x&y", b: "&z", c: "&w" } });
  });
});

// Inside a flow collection gopkg.in/yaml.v3 takes '?' as the explicit-key
// indicator whatever follows it; YAML 1.2 reads ?x as a plain scalar. So
// [?x] is ["?x"] here and [{"x": null}] there.
describe("a '?' starting a flow item", () => {
  const QUESTION = "Ambiguous '?' starting a flow item: quote the item.";

  it.each([
    ["a flow sequence item", "allowed_domains: [?x]", 1, 19],
    ["a flow mapping key", "scope: {?host: 1}", 1, 9],
    ["a flow pair key", "a: [?x: 1]", 1, 5],
    ["a '?' before a quoted JSON key", '{"a": [?"b"]}', 1, 8],
    ["a later flow item", "a: [y, ?z]", 1, 8],
  ])("refuses %s at the '?'", (_, source, line, column) => {
    expect(parsePolicySource(source)).toEqual({ ok: false, line, column, message: QUESTION });
  });

  it.each<[string, string, unknown]>([
    ["an explicit key with a space", "a: [? x]", { a: [{ x: null }] }],
    ["a block value", "a: ?x", { a: "?x" }],
    ["a block key", "?x: 1", { "?x": 1 }],
    ["a quoted flow item", 'a: ["?x"]', { a: ["?x"] }],
    ["a '?' later in a flow item", "a: [x?]", { a: ["x?"] }],
  ])("accepts %s", (_, source, value) => {
    expect(parsePolicySource(source)).toEqual({ ok: true, value });
  });
});

// With an explicit indentation indicator, a trailing whitespace-only line is
// content to gopkg.in/yaml.v3 (as YAML 1.2 says) and dropped here:
// `a: |1` over " x" and "  " is " x\n" here and " x\n \n" there.
describe("block scalar indentation indicators", () => {
  const INDICATOR = "Indentation indicators are not allowed: remove the digit after | or >.";

  it.each([
    ["a literal with an indicator", "a: |1\n  x\n  \nb: 1", 1, 4],
    ["a folded scalar with an indicator", "a: >1\n  x", 1, 4],
    ["an indicator after a chomping indicator", "a: >-1\n  x", 1, 4],
    ["an indicator before a chomping indicator", "a: |2+\n   x", 1, 4],
    ["an indicator in a sequence item", "a:\n  - |2\n     x", 2, 5],
  ])("refuses %s at the header", (_, source, line, column) => {
    expect(parsePolicySource(source)).toEqual({ ok: false, line, column, message: INDICATOR });
  });

  it.each<[string, string, unknown]>([
    ["a literal block", "a: |\n  x\n  y\n", { a: "x\ny\n" }],
    ["a folded block with chomping", "a: >-\n  x\n  y\n", { a: "x y" }],
    ["a kept literal block", "a: |+\n  x\n\n", { a: "x\n\n" }],
  ])("accepts %s", (_, source, value) => {
    expect(parsePolicySource(source)).toEqual({ ok: true, value });
  });
});

// An escaped line break followed by an empty line keeps that line as "\n" in
// gopkg.in/yaml.v3 (as YAML 1.2 says) and folds it to a space here.
describe("escaped line breaks in double-quoted strings", () => {
  const ESCAPED = "Escaped line break in a quoted string: write the string on one line.";

  it.each([
    ["one before an empty line", 'a: "x\\\n\n  y"', 1, 6],
    ["one before a continuation", 'a: "x\\\n  y"', 1, 6],
    ["one before CRLF", 'a: "x\\\r\n  y"', 1, 6],
    ["one in a key", '"k\\\n  z": 1', 1, 3],
    ["one after an escaped backslash", 'a: "x\\\\\\\n  y"', 1, 8],
  ])("refuses %s at the backslash", (_, source, line, column) => {
    expect(parsePolicySource(source)).toEqual({ ok: false, line, column, message: ESCAPED });
  });

  it.each<[string, string, unknown]>([
    ["an escaped backslash before a line break", 'a: "x\\\\\n  y"', { a: "x\\ y" }],
    ["a folded line break", 'a: "x\n  y"', { a: "x y" }],
    ["a \\n escape", 'a: "x\\ny"', { a: "x\ny" }],
    ["a single-quoted backslash before a line break", "a: 'x\\\n  y'", { a: "x\\ y" }],
  ])("accepts %s", (_, source, value) => {
    expect(parsePolicySource(source)).toEqual({ ok: true, value });
  });
});

// gopkg.in/yaml.v3 strips a byte order mark only at byte 0 and yaml here only
// before the first content line: `<LF><BOM>a: 1` reads as {"a": 1} here and
// {"<BOM>a": 1} there, and a doubled BOM the other way round.
describe("byte order marks", () => {
  const BOM = "Byte order mark inside the text: remove it, or write it as \\ufeff in a quoted string.";

  it.each<[string, PolicySourceFormat, string, number, number]>([
    ["one starting the second line", "yaml", "a: 1\n\ufeffb: 2", 2, 1],
    ["one after a comment line", "yaml", "# c\n\ufeffa: 1", 2, 1],
    ["a doubled leading one", "yaml", "\ufeff\ufeffa: 1", 1, 2],
    ["one inside a plain value", "yaml", "a: x\ufeffy", 1, 5],
    ["one inside a quoted value", "yaml", 'a: "x\ufeffy"', 1, 6],
    ["one inside a JSON string", "json", '{"a": "x\ufeffy"}', 1, 9],
  ])("refuses %s at the mark", (_, format, source, line, column) => {
    expect(parsePolicySource(source, format)).toEqual({ ok: false, line, column, message: BOM });
  });

  it("accepts one leading mark and the escaped form", () => {
    expect(parsePolicySource("\ufeffa: 1")).toEqual({ ok: true, value: { a: 1 } });
    expect(parsePolicySource('a: "x\\ufeffy"')).toEqual({ ok: true, value: { a: "x\ufeffy" } });
    expect(parsePolicySource('{"a": "x\\ufeffy"}', "json")).toEqual({ ok: true, value: { a: "x\ufeffy" } });
  });

  it.each<PolicySourceFormat>(["yaml", "json"])("never writes a raw mark from a %s edit", (format) => {
    const edited = editPolicySource("\ufeffa: 1\n", ["b"], ["x\ufeffy", "\ufefflead"], format);
    if (!edited.ok) throw new Error(edited.message);
    expect(edited.source.slice(1)).not.toMatch(/\ufeff/);
    expect(parsePolicySource(edited.source, format)).toEqual({ ok: true, value: { a: 1, b: ["x\ufeffy", "\ufefflead"] } });
  });
});
