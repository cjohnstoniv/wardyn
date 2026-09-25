/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import * as AdoEntraCopy from "./ado-entra-copy";
import { ADO } from "./ado-entra-copy";
import { parseFrozenTables, renderFromNamespaces, splitKey } from "./copy-doc-parity";

// The mock round's whole value is that it stays CHECKABLE (the drives/providers
// precedent, workspace-providers-copy.test.ts's parseFrozenTables()): this
// suite does not hand-retype a sample of the canon — it PARSES
// docs/design/ado-entra-prompt.md §7.2-§7.8 (AND §10, the capability card's
// own post-freeze addendum — S10 round 2/3) back out of the doc and compares
// every key. A swapped hyphen, a dropped ellipsis, a reworded clause, a new
// doc row or a deleted one all fail here rather than shipping.
//
// MUTATION-SAFE BY CONSTRUCTION, NOT BY A SEPARATE HARDCODED LIST (S10 round
// 3, N3): "covers every doc key" below takes EVERY key the doc parser finds,
// unconditionally — never filtered by what the module happens to export —
// and requires the module's own key set to equal it exactly in BOTH
// directions. An emptied ADO fails that assertion immediately (0 keys where
// the doc names 237), so the doc's own §7/§10 tables ARE the hardcoded
// expected-key list this suite is pinned against; there is no second list to
// keep in sync or let drift.
//
// The doc's own freeze note (§0) says §7.2 onward is 217 rows, but warns that
// is ITS OWN checker's count — this suite's parser is the one that matters.
// It agreed at 217 when §7 was frozen; CONNECT_POPUP_BLOCKED (review
// follow-up N1) added one row after the freeze at the same gate (218),
// CONNECT_POPUP_OPEN (#628's approved sign-in progress packet) one more (219),
// and §10's 23 rows (S10 round 2/3's 19 capability-card additions, plus §10.7's
// 2 rows for issue #458 — the not-applicable Settings card and the owner
// fallback) bring the live count to 242.
//
// Two normalisations, both documented rules rather than fudges (the drives
// precedent):
//   - BACKTICKS ARE STRIPPED from the doc cell. §7's header note makes mono a
//     DISPLAY concern applied by the consuming component; the frozen string
//     itself is plain text.
//   - A PARAMETERIZED key is called with its own placeholder text, so
//     REQ_PROTECTED_REF_TITLE("{ref}") must reproduce the doc's `{ref}` is
//     protected... character for character. The two pluralised keys
//     (CONSENT_SOME_MISSING, SAVED_NARROWED_RUNS) can't be checked that way
//     and get their own tests below.

// process.cwd() is the vitest root — ui/ — for every entry point that runs
// this suite (`pnpm vitest run`, `pnpm test`, make ci).
const DOC = resolve(process.cwd(), "../docs/design/ado-entra-prompt.md");

// §7.2-§7.8 — §7.1 is reused canon + the server-composed table (no Key
// column, and not this module's to carry) — plus §10's own subsections (the
// capability card's post-freeze addendum, S10 round 2/3).
const doc = parseFrozenTables(DOC, /^### (7\.[2-8]|10\.\d+)\b/);

// The keys whose doc cell carries an "A / B" pluralisation alternation rather
// than a single renderable string — checked in their own test below.
const PLURALISED = ["CONSENT_SOME_MISSING(n)", "SAVED_NARROWED_RUNS(n, capability)"];

const render = (docKey: string) => renderFromNamespaces(docKey, [ADO]);

const RENDERABLE = [...doc.keys()].filter((k) => !PLURALISED.includes(k));

describe("ado-entra-copy — §7.2-§7.8 parsed out of the prompt doc", () => {
  it("finds all 242 frozen keys in the doc (219 from §7, 23 from §10)", () => {
    expect(doc.size).toBe(242);
  });

  it("(#458) NOT_APPLICABLE_BODY and REQ_OWNER_FALLBACK are byte-exact", () => {
    expect(render("NOT_APPLICABLE_BODY")).toBe(doc.get("NOT_APPLICABLE_BODY"));
    expect(render("REQ_OWNER_FALLBACK")).toBe(doc.get("REQ_OWNER_FALLBACK"));
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    // BOTH directions: every doc row resolves to a module symbol, and every
    // module symbol has a doc row. A key added to the module and forgotten in
    // §7 fails here; a doc row with no module key fails too.
    const docNames = [...doc.keys()].map((k) => splitKey(k)[0]).sort();
    const moduleNames = Object.keys(ADO).sort();
    expect(moduleNames).toEqual(docNames);
  });

  it.each(RENDERABLE)("%s is byte-exact", (key) => {
    expect(render(key)).toBe(doc.get(key));
  });

  // The two pluralised keys. The doc cell spells BOTH arms separated by " / ";
  // each module function uses the inline ternary the providers precedent
  // uses, never a second pluralisation helper.
  it("CONSENT_SOME_MISSING renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("CONSENT_SOME_MISSING(n)")!.split(" / ");
    expect(ADO.CONSENT_SOME_MISSING(1)).toBe(singular.replace(/\{n\}/g, "1"));
    expect(ADO.CONSENT_SOME_MISSING(0)).toBe(plural.replace(/\{n\}/g, "0"));
    expect(ADO.CONSENT_SOME_MISSING(2)).toBe(plural.replace(/\{n\}/g, "2"));
  });

  it("SAVED_NARROWED_RUNS renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("SAVED_NARROWED_RUNS(n, capability)")!.split(" / ");
    expect(ADO.SAVED_NARROWED_RUNS(1, "push")).toBe(singular.replace(/\{n\}/g, "1").replace(/\{capability\}/g, "push"));
    expect(ADO.SAVED_NARROWED_RUNS(0, "push")).toBe(plural.replace(/\{n\}/g, "0").replace(/\{capability\}/g, "push"));
    expect(ADO.SAVED_NARROWED_RUNS(3, "push")).toBe(plural.replace(/\{n\}/g, "3").replace(/\{capability\}/g, "push"));
  });
});

describe("ado-entra-copy — the reuse and no-shadow rule §5 spells out", () => {
  // §5 #10 / §7.1: the module exports one namespace, ADO, and carries no copy
  // of §7.1's reused canon (PROVIDERS.*, APPROVAL_BANNER_LABEL.*, CAPABILITY.*,
  // MEMBER_GETTING_STARTED.*, OPERATOR_ONLY_REASON, PEOPLE.CANCEL) or of §7.1's
  // second table (the server-composed 400/422/403 refusals).
  it("exports exactly one namespace, ADO", () => {
    expect(Object.keys(AdoEntraCopy)).toEqual(["ADO"]);
  });

  it("carries no copy of the admin-facing server-composed refusals", () => {
    const all = JSON.stringify(ADO);
    for (const serverOnly of [
      "is in default_profile but not in capability_ceiling",
      "an Azure DevOps Server host signs in with a stored token",
      "Wardyn ties an Azure DevOps sign-in to the person's own session by matching the two",
      "tenant_id must be a GUID",
      "per_user needs the entra lane on this row",
      "creating and revoking Azure DevOps tokens is refused on every row",
      "you are not connected to Azure DevOps — connect and start the run again",
      "your Azure DevOps connection ended — connect and start the run again",
      "this run may only reach",
      "creating or revoking Azure DevOps tokens is refused for every run",
      "was denied for this run",
      "your organization's policy refuses this token",
    ]) {
      expect(all).not.toContain(serverOnly);
    }
  });
});
