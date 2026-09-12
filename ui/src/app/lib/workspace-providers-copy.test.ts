/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import * as WorkspaceProvidersCopy from "./workspace-providers-copy";
import { AGENTS, PROVIDER_MEMBER, PROVIDERS } from "./workspace-providers-copy";
import { MEMBER } from "./governance-copy";
import { DRIVES, DRIVE_MEMBER, DRIVE_RUN } from "./user-drives-copy";

// The mock round's whole value is that it stays CHECKABLE (the drives
// precedent, user-drives-copy.test.ts's parseFrozenTables()): this suite does
// not hand-retype a sample of the canon — it PARSES docs/design/
// workspace-providers-prompt.md §7.2-§7.5 + §7.7 back out of the doc and
// compares all 91 keys. A swapped hyphen, a dropped ellipsis, a reworded
// clause, a new doc row or a deleted one all fail here rather than shipping.
//
// §7.6 is STAGING (field-report strings owned by other lanes, parsed by
// nothing today per the doc's own header note) and is excluded by the doc's
// stated regex: /^### 7\.[2-57]\b/ matches 7.2, 7.3, 7.4, 7.5, 7.7 — not 7.6.
//
// Two normalisations, both documented rules rather than fudges (the drives
// precedent):
//   - BACKTICKS ARE STRIPPED from the doc cell. §7's header note makes mono a
//     DISPLAY concern applied by the consuming component; the frozen string
//     itself is plain text.
//   - A PARAMETERIZED key is called with its own placeholder text, so
//     REMOVE_CONFIRM("{kind}") must reproduce the doc's `Remove the {kind}
//     row?…` character for character. The pluralised keys (§5 #9) can't be
//     checked that way and get their own tests below.

// process.cwd() is the vitest root — ui/ — for every entry point that runs
// this suite (`pnpm vitest run`, `pnpm test`, make ci).
const DOC = resolve(process.cwd(), "../docs/design/workspace-providers-prompt.md");

const unmono = (s: string) => s.replace(/`/g, "");

/** key -> frozen string, for every row of §7.2-§7.5 + §7.7's tables (§7.6 excluded). */
function parseFrozenTables(): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      // §7.2-§7.5 + §7.7 only — the doc's own stated regex (§9.3's clone note):
      // §7.1 is reused canon + the server-composed table (no Key column, and
      // not this module's to carry); §7.6 is the M2-sitting staging table,
      // parsed by nothing until each row lands with its own lane.
      inSection = /^### 7\.[2-57]\b/.test(line);
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

// The keys whose doc cell carries an "A / B" pluralisation alternation rather
// than a single renderable string (§5 #9) — checked in their own test below.
const PLURALISED = ["SAVED_NARROWED(n)", "CARD_PROVIDERS(n)", "CARD_AGENTS(n)", "STEP_BADGE_READY(n)", "STEP_BADGE_READY_AGENTS(n)"];

// Every other key is rendered FROM THE DOC's own key cell rather than from a
// hand-typed table (the drives precedent): `REMOVE_CONFIRM(kind)` is split
// into the module symbol and its argument names, and the value is called with
// "{kind}" so it must reproduce the doc cell character for character.
const NAMESPACES: Record<string, unknown>[] = [PROVIDERS, PROVIDER_MEMBER, AGENTS];

/** `REMOVE_CONFIRM(kind)` -> ["REMOVE_CONFIRM", ["kind"]]. */
function splitKey(docKey: string): [string, string[]] {
  const m = /^([A-Z0-9_]+)\((.*)\)$/.exec(docKey);
  return m ? [m[1], m[2].split(",").map((a) => a.trim())] : [docKey, []];
}

// ONE lookup across the three namespaces is safe because none of their keys
// collide (56 / 3 / 32); the completeness test below is what keeps that true.
function render(docKey: string): string {
  const [name, args] = splitKey(docKey);
  const ns = NAMESPACES.find((n) => name in n);
  if (!ns) throw new Error(`${docKey}: no such key in PROVIDERS / PROVIDER_MEMBER / AGENTS`);
  const value = ns[name];
  return typeof value === "function" ? (value as (...a: string[]) => string)(...args.map((a) => `{${a}}`)) : String(value);
}

const RENDERABLE = [...doc.keys()].filter((k) => !PLURALISED.includes(k));

describe("workspace-providers-copy — §7.2-§7.5 + §7.7 parsed out of the prompt doc", () => {
  it("finds all 91 frozen keys in the doc (§7.6 excluded)", () => {
    expect(doc.size).toBe(91);
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    // BOTH directions: every doc row resolves to a module symbol, and every
    // module symbol has a doc row. A key added to the module and forgotten in
    // §7 fails here; a doc row with no module key fails too.
    const docNames = [...doc.keys()].map((k) => splitKey(k)[0]).sort();
    const moduleNames = NAMESPACES.flatMap((n) => Object.keys(n)).sort();
    expect(moduleNames).toEqual(docNames);
  });

  it.each(RENDERABLE)("%s is byte-exact", (key) => {
    expect(render(key)).toBe(doc.get(key));
  });

  // The pluralised keys (§5 #9). The doc cell spells BOTH arms separated by
  // " / "; each module function uses the inline ternary PERM.ENFORCE_ON_BODY
  // uses, never a second pluralisation helper.
  it("SAVED_NARROWED renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("SAVED_NARROWED(n)")!.split(" / ");
    expect(PROVIDERS.SAVED_NARROWED(1)).toBe(singular.replace(/\{n\}/g, "1"));
    expect(PROVIDERS.SAVED_NARROWED(0)).toBe(plural.replace(/\{n\}/g, "0"));
    expect(PROVIDERS.SAVED_NARROWED(2)).toBe(plural.replace(/\{n\}/g, "2"));
  });

  it("CARD_PROVIDERS renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("CARD_PROVIDERS(n)")!.split(" / ");
    expect(PROVIDERS.CARD_PROVIDERS(1)).toBe(singular.replace("{n}", "1"));
    expect(PROVIDERS.CARD_PROVIDERS(0)).toBe(plural.replace("{n}", "0"));
    expect(PROVIDERS.CARD_PROVIDERS(2)).toBe(plural.replace("{n}", "2"));
  });

  it("CARD_AGENTS renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("CARD_AGENTS(n)")!.split(" / ");
    expect(PROVIDERS.CARD_AGENTS(1)).toBe(singular.replace("{n}", "1"));
    expect(PROVIDERS.CARD_AGENTS(0)).toBe(plural.replace("{n}", "0"));
    expect(PROVIDERS.CARD_AGENTS(3)).toBe(plural.replace("{n}", "3"));
  });

  it("STEP_BADGE_READY renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("STEP_BADGE_READY(n)")!.split(" / ");
    expect(PROVIDERS.STEP_BADGE_READY(1)).toBe(singular.replace("{n}", "1"));
    expect(PROVIDERS.STEP_BADGE_READY(0)).toBe(plural.replace("{n}", "0"));
    expect(PROVIDERS.STEP_BADGE_READY(2)).toBe(plural.replace("{n}", "2"));
  });

  it("STEP_BADGE_READY_AGENTS renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("STEP_BADGE_READY_AGENTS(n)")!.split(" / ");
    expect(PROVIDERS.STEP_BADGE_READY_AGENTS(1)).toBe(singular.replace("{n}", "1"));
    expect(PROVIDERS.STEP_BADGE_READY_AGENTS(0)).toBe(plural.replace("{n}", "0"));
    expect(PROVIDERS.STEP_BADGE_READY_AGENTS(4)).toBe(plural.replace("{n}", "4"));
  });
});

describe("workspace-providers-copy — the reuse and no-shadow rules §5 spells out", () => {
  // §5 #10: "This module exports no name user-drives-copy.ts or
  // governance-copy.ts exports." Its namespaces are PROVIDERS, AGENTS and
  // PROVIDER_MEMBER — none of which either module originates.
  it("PROVIDERS / PROVIDER_MEMBER / AGENTS collide with none of user-drives-copy.ts's or governance-copy.ts's own namespace names", () => {
    const taken = ["DRIVES", "DRIVE_MEMBER", "DRIVE_RUN", "GOVERNANCE", "MEMBER", "POSITIONING", "DIRECTORY"];
    for (const name of Object.keys(WorkspaceProvidersCopy)) {
      if (taken.includes(name)) {
        throw new Error(`workspace-providers-copy.ts exports "${name}", which another copy module already exports`);
      }
    }
    expect(["PROVIDERS", "PROVIDER_MEMBER", "AGENTS"].every((n) => Object.keys(WorkspaceProvidersCopy).includes(n))).toBe(true);
  });

  // PROVIDER_MEMBER renders on the SAME launch/refusal surfaces DRIVE_MEMBER
  // and MEMBER already do — a key collision between the three objects' OWN
  // keys is the concrete risk the no-shadow rule guards against.
  it("PROVIDER_MEMBER shares no key with governance-copy.ts's MEMBER or user-drives-copy.ts's DRIVE_MEMBER", () => {
    const overlapMember = Object.keys(PROVIDER_MEMBER).filter((k) => Object.keys(MEMBER).includes(k));
    const overlapDrive = Object.keys(PROVIDER_MEMBER).filter((k) => Object.keys(DRIVE_MEMBER).includes(k));
    expect(overlapMember).toEqual([]);
    expect(overlapDrive).toEqual([]);
  });

  // PROVIDERS and DRIVES are two separate admin screens that deliberately
  // freeze the SAME short field names for the same concept (TITLE,
  // FIELD_ENABLED, SAVE_CTA/SAVE_ERROR/SAVE_REFUSED_TITLE,
  // FETCH_FAILED_TITLE/_BODY, CARD_LEAD/_EMPTY/_SUMMARY/_OPEN, REMOVE_CONFIRM)
  // — the drives precedent (user-drives-copy.test.ts's own "documents that
  // DRIVES intentionally reuses GOVERNANCE's inner key names"): §5 #10's
  // "no name" guarantee is the NAMESPACE name (checked above), not every
  // inner key, since PROVIDERS and DRIVES are never imported at one call site
  // the way PROVIDER_MEMBER and DRIVE_MEMBER would be.
  it("documents that PROVIDERS intentionally reuses DRIVES's inner key names", () => {
    const all = new Set([...Object.keys(DRIVES), ...Object.keys(DRIVE_MEMBER), ...Object.keys(DRIVE_RUN)]);
    const overlap = Object.keys(PROVIDERS).filter((k) => all.has(k));
    expect(overlap.length).toBeGreaterThan(0);
  });

  // §7.1 / Hard canon #10: the module deliberately carries NO copy of the
  // admin-facing and run-time strings the server composes for THIS round
  // (§7.1's second table) — those are rendered from the wire only.
  it("carries no copy of the admin-facing server-composed refusals", () => {
    const all = JSON.stringify(PROVIDERS) + JSON.stringify(PROVIDER_MEMBER) + JSON.stringify(AGENTS);
    for (const serverOnly of [
      "must be an https URL with a host and no credentials",
      "is not a github host",
      "is not available for azure_devops",
      "may not exceed max_disk_mib",
      "is not unique",
      "outside every enabled provider's allowed addresses",
      "was not wired",
      "legacy scm_hosts list",
      "exceeds this deployment's drive ceiling",
      "disabled for this deployment",
      "requires TLS",
      "names no agent this deployment can run",
      "its mechanism must be none",
      "per_user is available for bedrock_sso only",
      "sso_start_url is required",
      "no credential is configured for this brokered LLM route",
      "below policy",
    ]) {
      expect(all).not.toContain(serverOnly);
    }
  });
});
