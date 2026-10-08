/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The display emitter (code-block's toYaml) and the strict parser must agree: a
// generated YAML view that reads back as a different mapping would change the
// policy a run launches with.
import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { toYaml } from "../../components/wardyn/code-block";
import { policyTemplates } from "../../components/wardyn/policy-panel";
import { DEMOS } from "../../components/screens/demos/demo-catalog";
import { SECRETS_DEMOS } from "../../components/screens/demos/demo-catalog-secrets";
import { defaultSpecText } from "../../components/screens/new-run/policy-lane";
import type { SetupModelProvider } from "../types";
import { parsePolicySource, type PolicySourceMapping, type PolicySourceValue } from ".";

function expectRoundTrip(value: PolicySourceMapping) {
  const parsed = parsePolicySource(toYaml(value));
  expect(parsed).toEqual({ ok: true, value });
  if (!parsed.ok) throw new Error(parsed.message);
  expect(JSON.stringify(parsed.value)).toBe(JSON.stringify(value));
  // The same value written as JSON (line-break characters escaped, as every
  // writer here does) must stay acceptable to explicit JSON mode.
  const json = JSON.stringify(value, null, 2).replace(/[\x85\u2028\u2029\ufeff]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0")}`);
  expect(parsePolicySource(json, "json")).toEqual({ ok: true, value });
}

const STRINGS = [
  "", " ", "  leading", "trailing  ", "\tx", "x\t", "in\tner", "echo one\necho two", "\n", "x\n", "\nx", "a\r\nb", "a\rb",
  "\u0000", "bell\u0007", "esc\u001b[0m", "del\u007f", "nel\u0085x", "c1\u009fx", "ls\u2028x", "ps\u2029x", "\ufeffbom",
  "x\ufeffy", "nb\u00a0sp", "\u00a0", "é😀", "\ud800", "a: b", "a:b", "a:", ":a", "a #b", "a#b", "#a", "- a", "-a", "-",
  "? a", "?a", "'single'", '"double"', "it's", 'say "hi"', "back\\slash", "\\n", "[a]", "{a}", "a,b", "&a", "*a", "!a",
  "|", ">", "%a", "@a", "`a`", "<<", "=", "a=b", "true", "True", "TRUE", "false", "null", "Null", "NULL", "~", "yes",
  "No", "on", "OFF", "y", "n", "0", "-1", "+1", "017", "0o17", "0x1f", "1e3", "1.5", ".5", "1_000", "1:30",
  "2000-10-07", "2001-12-14 21:59:43.10 -5", "+_1", "+_.5", "+__1e3", "+_0x1F", "_1", "-_1", ".nan", ".NaN", ".inf", "-.inf", "+.Inf", "NaN", "Infinity", "---", "...",
  "--- a", "%YAML 1.1", "!!binary aGk=", "*missing", "&anchor x", "key: [flow, {a: b}]", "# comment", "a\n# b\n- c\n",
  "https://github.com/acme/one", "*.githubusercontent.com", "Bearer %s", "rm\\s+-rf\\b", "api.anthropic.com",
  "__proto__", "constructor", "x".repeat(1024), "x".repeat(1025), "\n".repeat(600),
];
const KEYS = STRINGS.filter((key) => key !== "<<");
const NUMBERS = [0, 1, -1, 3600, 1.5, -0.25, 0.1, 1e-7, -2.5e-10, 123456.789, 5e-324, 2 ** 31, Number.MAX_SAFE_INTEGER, Number.MIN_SAFE_INTEGER];
const FIELDS = ["allowed_domains", "eligible_grants", "scope", "repos", "permissions", "tool_rules", "pattern", "llm_inspection", "classified_markers"];
const ALPHABET = [..." \t\n\r:#-?'\"\\[]{},&*!|>%@`~<=.+0aTé\u2028\u0000😀"];

// mulberry32: a fixed seed keeps the generated corpus identical on every run.
function seeded(seed: number): () => number {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function generate(random: () => number, depth: number): PolicySourceValue {
  const pick = <T>(items: readonly T[]): T => items[Math.floor(random() * items.length)];
  const roll = random();
  if (depth > 3 || roll < 0.45) {
    if (roll < 0.2) return pick(STRINGS);
    if (roll < 0.3) return Array.from({ length: 1 + Math.floor(random() * 12) }, () => pick(ALPHABET)).join("");
    return pick([null, true, false, ...NUMBERS]);
  }
  const items = Array.from({ length: Math.floor(random() * 5) }, () => generate(random, depth + 1));
  if (roll < 0.7) return items;
  return Object.fromEntries(items.map((item) => [pick(random() < 0.5 ? FIELDS : KEYS), item]));
}

describe("toYaml output read back by parsePolicySource", () => {
  it.each(STRINGS.map((text) => [JSON.stringify(text.length > 40 ? `${text.slice(0, 12)}… (${text.length})` : text), text]))(
    "round-trips the string %s as a value, a list item and a nested value",
    (_, text) => expectRoundTrip({ value: text, list: [text, [text], { nested: text }], map: { nested: { deeper: text } } }),
  );

  it.each(KEYS.map((key) => [JSON.stringify(key.length > 40 ? `${key.slice(0, 12)}… (${key.length})` : key), key]))(
    "round-trips the mapping key %s at the root, nested and inside a list",
    (_, key) => expectRoundTrip({ [key]: "scalar", nested: { [key]: { [key]: [key] } }, list: [{ [key]: 1, after: 2 }, { first: 1, [key]: [] }] }),
  );

  it("round-trips empty collections and nested lists of maps and maps of lists", () => {
    expectRoundTrip({});
    expectRoundTrip({ a: {}, b: [], c: [[], {}, [[]], [{}]], d: { e: { f: {} }, g: [] } });
    expectRoundTrip({ a: [[1, 2], [3, [4, [5, { b: [6, { c: [] }] }]]]], d: [{ e: [{ f: [{ g: 1 }, 2] }, 3] }, 4] });
    expectRoundTrip({ a: [{ b: 1, c: [true, null] }, { d: { e: ["x"] } }, [{ f: 1 }, { g: 2 }]] });
    for (const value of NUMBERS) expectRoundTrip({ value, list: [value] });
  });

  it("round-trips multi-line llm_inspection.classified_markers", () => {
    expectRoundTrip({
      llm_inspection: {
        mode: "block",
        classified_markers: ["TOP SECRET//SCI", "line one\nline two", "-----BEGIN KEY-----\nabc\n-----END KEY-----\n", "tab\tand # hash: colon", "  padded  "],
      },
    });
  });

  it("round-trips a seeded corpus of policy-shaped values", () => {
    const random = seeded(0x1921);
    for (let index = 0; index < 400; index++) {
      const value = generate(random, 0);
      expectRoundTrip({ spec: value, [KEYS[index % KEYS.length]]: generate(random, 2) });
    }
  });

  it("writes negative zero as the 0 JSON sends on the wire", () => {
    expect(toYaml({ a: -0 })).toBe("a: 0");
    expect(JSON.stringify({ a: -0 })).toBe('{"a":0}');
  });

  it.each(["<<", "nested"])("never turns a merge key under %s into something the parser accepts", (outer) => {
    const value = outer === "<<" ? { "<<": { a: 1 } } : { nested: [{ "<<": "x" }] };
    const parsed = parsePolicySource(toYaml(value));
    expect(parsed).toMatchObject({ ok: false, message: "Merge keys (<<) are not allowed." });
    expect(parsed).not.toHaveProperty("value");
  });
});

const repo = path.resolve(__dirname, "../../../../..");
const read = (file: string) => fs.readFileSync(path.join(repo, file), "utf8");
const exampleFiles = fs.readdirSync(path.join(repo, "examples/policies")).filter((file) => /\.(json|ya?ml)$/.test(file)).sort();
const docFences = [...read("docs/POLICIES.md").matchAll(/```json\n([\s\S]*?)```/g)].map((match) => match[1]);
const provider = (kind: string, host: string): SetupModelProvider => ({ id: kind, kind, host, harnesses: ["claude-code"] });
const PROVIDER_SETS: (SetupModelProvider[] | undefined)[] = [
  undefined,
  [provider("anthropic_api_key", "api.anthropic.com"), provider("openai_api_key", "api.openai.com")],
  [provider("bedrock_sso", "bedrock-runtime.us-east-1.amazonaws.com")],
];

describe("shipped policy specimens", () => {
  it("finds the specimens it claims to cover", () => {
    expect(exampleFiles.length).toBeGreaterThanOrEqual(12);
    expect(docFences.length).toBeGreaterThanOrEqual(1);
  });

  it.each([
    ...exampleFiles.map((file) => [`examples/policies/${file}`, read(`examples/policies/${file}`)]),
    ["deploy/kind/sso/default-policy.json", read("deploy/kind/sso/default-policy.json")],
    ...docFences.map((source, index) => [`docs/POLICIES.md json block ${index + 1}`, source]),
  ])("round-trips %s", (_, source) => {
    const authored = parsePolicySource(source);
    if (!authored.ok) throw new Error(`${authored.line}:${authored.column} ${authored.message}`);
    expectRoundTrip(authored.value);
  });

  it("round-trips every template, the default custom policy and every demo policy", () => {
    const specs: unknown[] = [...DEMOS, ...SECRETS_DEMOS].map((demo) => demo.policy);
    for (const providers of PROVIDER_SETS) {
      specs.push(JSON.parse(defaultSpecText(providers)), ...policyTemplates(providers).map((template) => template.spec));
    }
    expect(specs.length).toBeGreaterThan(30);
    for (const spec of specs) expectRoundTrip(spec as PolicySourceMapping);
  });
});
