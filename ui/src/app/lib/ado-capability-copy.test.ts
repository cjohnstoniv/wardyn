/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { ADO_CAPABILITY } from "./ado-capability-copy";

// This is the SAME parseFrozenTables() method ado-entra-copy.test.ts (PR
// #415) uses: parse docs/design/ado-entra-prompt.md's frozen §7 tables (and
// this round's own §10 addendum — see ado-capability-copy.ts's top comment)
// back out of the doc and compare every key this module carries, so a
// swapped hyphen or a reworded clause fails here instead of shipping.
//
// ROUND-2 FIX (independent review F5): the first version of this test
// derived its "expected keys" BY FILTERING THE MODULE ITSELF (every doc key
// whose name existed in ADO_CAPABILITY) — which means an EMPTY module
// produced an EMPTY expected-key set and the coverage assertion passed
// vacuously. EXPECTED below is a hand-written, hardcoded list, entirely
// independent of what the module happens to export, so emptying the module
// (or silently dropping a key) fails "the module exports exactly these keys"
// rather than passing by shrinking both sides of the comparison together.
const DOC = resolve(process.cwd(), "../docs/design/ado-entra-prompt.md");

const unmono = (s: string) => s.replace(/`/g, "");

/** key -> frozen string, for every row of §7.4, §7.6, §7.8 and §10's tables. */
function parseFrozenTables(): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      inSection = /^### 7\.(4|6|8)\b/.test(line) || /^### 10\.\d+\b/.test(line);
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key") continue; // header
    if (/^:?-+:?$/.test(cells[0])) continue; // separator
    rows.set(unmono(cells[0]), unmono(cells[cells.length - 1]));
  }
  return rows;
}

const doc = parseFrozenTables();

/** `REQ_WAITING_OTHER(person)` -> ["REQ_WAITING_OTHER", ["person"]]. */
function splitKey(docKey: string): [string, string[]] {
  const m = /^([A-Z0-9_]+)\((.*)\)$/.exec(docKey);
  return m ? [m[1], m[2].split(",").map((a) => a.trim())] : [docKey, []];
}

function render(name: string, args: string[]): string {
  const value = (ADO_CAPABILITY as Record<string, unknown>)[name];
  if (value === undefined) throw new Error(`${name}: no such key in ADO_CAPABILITY`);
  return typeof value === "function" ? (value as (...a: string[]) => string)(...args) : String(value);
}

// The hardcoded expected surface — every key ADO_CAPABILITY is SUPPOSED to
// carry, as (module key name, its doc key with placeholder arg names). This
// list is the test's ground truth, not the module's.
const EXPECTED: Array<[string, string]> = [
  ["CAP_READ", "CAP_READ"],
  ["CAP_CODE_WRITE", "CAP_CODE_WRITE"],
  ["CAP_PR", "CAP_PR"],
  ["CAP_POLICY_ADMIN", "CAP_POLICY_ADMIN"],
  ["CAP_POLICY_BYPASS", "CAP_POLICY_BYPASS"],
  ["CAP_BUILD_EXECUTE", "CAP_BUILD_EXECUTE"],
  ["CAP_REPO_ADMIN", "CAP_REPO_ADMIN"],
  ["CAP_WORK_WRITE", "CAP_WORK_WRITE"],
  ["CAP_WIKI_WRITE", "CAP_WIKI_WRITE"],
  ["REQ_WAITING", "REQ_WAITING"],
  ["REQ_WAITING_OTHER", "REQ_WAITING_OTHER(person)"],
  ["REQ_SOURCE", "REQ_SOURCE(ts)"],
  ["REQ_FIELD_REPOSITORY", "REQ_FIELD_REPOSITORY"],
  ["REQ_FIELD_COMMAND", "REQ_FIELD_COMMAND"],
  ["REQ_FIELD_REQUEST", "REQ_FIELD_REQUEST"],
  ["REQ_FIELD_ACTS_AS", "REQ_FIELD_ACTS_AS"],
  ["REQ_ACTS_AS_HINT", "REQ_ACTS_AS_HINT(person)"],
  ["REQ_HELD", "REQ_HELD(thing)"],
  ["REQ_SCOPE_READOUT", "REQ_SCOPE_READOUT(scope)"],
  ["REQ_APPROVING_ONCE", "REQ_APPROVING_ONCE(thing)"],
  ["REQ_APPROVING_RUN", "REQ_APPROVING_RUN(thing)"],
  ["REQ_DENYING", "REQ_DENYING(thing)"],
  ["REQ_SCOPE_ONCE_HINT", "REQ_SCOPE_ONCE_HINT(thing)"],
  ["REQ_SCOPE_RUN_HINT", "REQ_SCOPE_RUN_HINT(thing)"],
  ["REQ_SCOPE_UNTIL_REFUSED", "REQ_SCOPE_UNTIL_REFUSED"],
  ["REQ_SCOPE_ALWAYS_REFUSED", "REQ_SCOPE_ALWAYS_REFUSED"],
  ["REQ_CONSENT_CHIP", "REQ_CONSENT_CHIP"],
  ["REQ_CONSENT_BODY", "REQ_CONSENT_BODY"],
  ["REQ_CONSENT_CTA", "REQ_CONSENT_CTA"],
  ["REQ_CONSENT_OTHER_BODY", "REQ_CONSENT_OTHER_BODY(person)"],
  ["REQ_NOT_YOURS_CHIP", "REQ_NOT_YOURS_CHIP"],
  ["REQ_NOT_YOURS_BODY", "REQ_NOT_YOURS_BODY(person)"],
  ["LIST_ENDED_CHIP", "LIST_ENDED_CHIP"],
  ["LIST_ENDED_BODY", "LIST_ENDED_BODY"],
  ["OUTCOME_ALLOWED_ONCE", "OUTCOME_ALLOWED_ONCE"],
  ["OUTCOME_ALLOWED_RUN", "OUTCOME_ALLOWED_RUN"],
  ["TOOL_CALL_NOTE", "TOOL_CALL_NOTE"],
  ["CAP_THING_READ", "CAP_THING_READ"],
  ["CAP_THING_CODE_WRITE", "CAP_THING_CODE_WRITE"],
  ["CAP_THING_PR", "CAP_THING_PR"],
  ["CAP_THING_POLICY_ADMIN", "CAP_THING_POLICY_ADMIN"],
  ["CAP_THING_POLICY_BYPASS", "CAP_THING_POLICY_BYPASS"],
  ["CAP_THING_REPO_ADMIN", "CAP_THING_REPO_ADMIN"],
  ["CAP_THING_BUILD_EXECUTE", "CAP_THING_BUILD_EXECUTE"],
  ["CAP_THING_WORK_WRITE", "CAP_THING_WORK_WRITE"],
  ["CAP_THING_WIKI_WRITE", "CAP_THING_WIKI_WRITE"],
  ["REQ_FIELD_REF_CLASS", "REQ_FIELD_REF_CLASS"],
  ["REQ_REF_CLASS_PROTECTED", "REQ_REF_CLASS_PROTECTED"],
  ["REQ_CONSENT_HEADING", "REQ_CONSENT_HEADING"],
  ["REQ_HELD_EXPIRED", "REQ_HELD_EXPIRED(thing)"],
  ["REQ_RUN_UNAVAILABLE", "REQ_RUN_UNAVAILABLE"],
  ["SCOPE_UNTIL_LABEL", "SCOPE_UNTIL_LABEL"],
  ["SCOPE_ALWAYS_LABEL", "SCOPE_ALWAYS_LABEL"],
  ["STRIP_HEADING_CONSENT", "STRIP_HEADING_CONSENT"],
  ["WAITING_ADO_MINE", "WAITING_ADO_MINE"],
  ["WAITING_ADO_OWNER", "WAITING_ADO_OWNER"],
];

describe("ado-capability-copy — the hardcoded expected surface", () => {
  it("is not empty (a mutation guard: an emptied module must fail this suite)", () => {
    expect(EXPECTED.length).toBeGreaterThan(0);
    expect(EXPECTED.length).toBe(56);
  });

  it("the module exports EXACTLY the expected keys — no fewer, no more", () => {
    expect(Object.keys(ADO_CAPABILITY).sort()).toEqual(EXPECTED.map(([k]) => k).sort());
  });

  it("every expected key exists, is non-empty, and is found in the doc under §7.4/§7.6/§7.8/§10", () => {
    for (const [moduleKey, docKey] of EXPECTED) {
      const value = (ADO_CAPABILITY as Record<string, unknown>)[moduleKey];
      expect(value, `${moduleKey} is missing from the module`).toBeDefined();
      if (typeof value === "string") expect(value.length, `${moduleKey} is empty`).toBeGreaterThan(0);
      expect(doc.has(docKey), `${docKey} not found in the doc's §7.4/§7.6/§7.8/§10 tables`).toBe(true);
    }
  });

  it.each(EXPECTED)("%s is byte-exact against the doc", (_moduleKey, docKey) => {
    const [name, placeholders] = splitKey(docKey);
    expect(render(name, placeholders.map((p) => `{${p}}`))).toBe(doc.get(docKey));
  });

  it("carries no key the doc's §7.4/§7.6/§7.8/§10 tables don't have a row for", () => {
    const docNames = new Set([...doc.keys()].map((k) => splitKey(k)[0]));
    for (const key of Object.keys(ADO_CAPABILITY)) {
      expect(docNames.has(key), `${key} has no matching doc row`).toBe(true);
    }
  });
});
