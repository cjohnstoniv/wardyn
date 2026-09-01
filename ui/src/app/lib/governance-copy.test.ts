/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { DIRECTORY, GOVERNANCE, MEMBER, POSITIONING } from "./governance-copy";
import { PEOPLE } from "./people-access-copy";

// The mock round's whole value is that it stays CHECKABLE, so this suite does
// not hand-retype a sample of the canon — it PARSES docs/design/
// governance-prompt.md §7.2-§7.9 back out of the doc and compares all 85 keys.
// A swapped hyphen, a dropped ellipsis, a reworded clause, a new doc row or a
// deleted one all fail here rather than shipping.
//
// Two normalisations, both of them documented rules rather than fudges:
//   - BACKTICKS ARE STRIPPED from the doc cell. §7's header note makes mono a
//     DISPLAY concern applied by the consuming component (people-access-
//     copy.ts's own backtick-mono rule); the frozen string itself is plain
//     text. Nothing in §7.2-§7.9 contains a backtick as content.
//   - A PARAMETERIZED key is called with its own placeholder text, so
//     EDITOR_TITLE_EDIT("{name}") must reproduce the doc's `Edit "{name}"`
//     character for character. The two inline-pluralised keys can't be
//     checked that way and get their own test below.

// process.cwd() is the vitest root — ui/ — for every entry point that runs
// this suite (`pnpm vitest run`, `pnpm test`, make ci). import.meta.url is not
// a file: URL under the jsdom environment, so it can't resolve this.
const DOC = resolve(process.cwd(), "../docs/design/governance-prompt.md");

const unmono = (s: string) => s.replace(/`/g, "");

/** key -> frozen string, for every row of §7.2-§7.9's tables. */
function parseFrozenTables(): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      // §7.2 onward only: §7.1 is the reused-canon table (strings that live in
      // permissions-copy.ts / people-access-copy.ts / the server), not keys
      // this module freezes.
      inSection = /^### 7\.[2-9]\b/.test(line);
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key") continue; // header
    if (/^:?-+:?$/.test(cells[0])) continue; // separator
    // §7.8 is a three-column table (Key | Today | Frozen) — the FROZEN column
    // is the canon; "Today" is the string being replaced.
    rows.set(unmono(cells[0]), unmono(cells[cells.length - 1]));
  }
  return rows;
}

const doc = parseFrozenTables();

// The two keys whose doc cell carries an "A / B" pluralisation alternation
// rather than a single renderable string — checked in their own test.
const PLURALISED = ["ASSIGNED_COUNT(n)", "DELETE_RESTRICT_BODY(name, n)"];

// Every other key, rendered from the module exactly as the doc spells it.
const rendered: Record<string, string> = {
  // ---- §7.2 ----
  TITLE: GOVERNANCE.TITLE,
  LEAD: GOVERNANCE.LEAD,
  PROFILES_TITLE: GOVERNANCE.PROFILES_TITLE,
  PROFILES_LEAD: GOVERNANCE.PROFILES_LEAD,
  COL_NAME: GOVERNANCE.COL_NAME,
  COL_ASSIGNED: GOVERNANCE.COL_ASSIGNED,
  COL_LIMITS: GOVERNANCE.COL_LIMITS,
  COL_GRADE: GOVERNANCE.COL_GRADE,
  COL_UPDATED: GOVERNANCE.COL_UPDATED,
  NEW_CTA: GOVERNANCE.NEW_CTA,
  EDIT: GOVERNANCE.EDIT,
  DELETE: GOVERNANCE.DELETE,
  ASSIGNED_NONE: GOVERNANCE.ASSIGNED_NONE,
  LIMITS_NONE: GOVERNANCE.LIMITS_NONE,
  EMPTY_TITLE: GOVERNANCE.EMPTY_TITLE,
  EMPTY_BODY: GOVERNANCE.EMPTY_BODY,
  EDITOR_TITLE_NEW: GOVERNANCE.EDITOR_TITLE_NEW,
  "EDITOR_TITLE_EDIT(name)": GOVERNANCE.EDITOR_TITLE_EDIT("{name}"),
  FIELD_NAME: GOVERNANCE.FIELD_NAME,
  NAME_HINT: GOVERNANCE.NAME_HINT,
  CEILING_TITLE: GOVERNANCE.CEILING_TITLE,
  CEILING_LEAD: GOVERNANCE.CEILING_LEAD,
  LIMITS_TITLE: GOVERNANCE.LIMITS_TITLE,
  LIMITS_LEAD: GOVERNANCE.LIMITS_LEAD,
  LIMIT_EXEC_LABEL: GOVERNANCE.LIMIT_EXEC_LABEL,
  LIMIT_EXEC_HINT: GOVERNANCE.LIMIT_EXEC_HINT,
  LIMIT_INTERACTIVE_LABEL: GOVERNANCE.LIMIT_INTERACTIVE_LABEL,
  LIMIT_INTERACTIVE_HINT: GOVERNANCE.LIMIT_INTERACTIVE_HINT,
  GRADE_NOTE: GOVERNANCE.GRADE_NOTE,
  SAVE: GOVERNANCE.SAVE,
  SAVE_ERROR: GOVERNANCE.SAVE_ERROR,

  // ---- §7.3 ----
  ASSIGN_TITLE: GOVERNANCE.ASSIGN_TITLE,
  ASSIGN_LEAD: GOVERNANCE.ASSIGN_LEAD,
  PRECEDENCE: GOVERNANCE.PRECEDENCE,
  EFFECT_NOTE: GOVERNANCE.EFFECT_NOTE,
  SIGNIN_NOTE: GOVERNANCE.SIGNIN_NOTE,
  COL_PROFILE: GOVERNANCE.COL_PROFILE,
  COL_PRIORITY: GOVERNANCE.COL_PRIORITY,
  PRIORITY_NA: GOVERNANCE.PRIORITY_NA,
  PRIORITY_HINT: GOVERNANCE.PRIORITY_HINT,
  ADD_TITLE: GOVERNANCE.ADD_TITLE,
  ADD_CTA: GOVERNANCE.ADD_CTA,
  FIELD_PROFILE: GOVERNANCE.FIELD_PROFILE,
  PROFILE_PLACEHOLDER: GOVERNANCE.PROFILE_PLACEHOLDER,
  FIELD_PRIORITY: GOVERNANCE.FIELD_PRIORITY,
  "UNASSIGN_CONFIRM(who, name)": GOVERNANCE.UNASSIGN_CONFIRM("{who}", "{name}"),
  EMPTY_ASSIGN_TITLE: GOVERNANCE.EMPTY_ASSIGN_TITLE,
  EMPTY_ASSIGN_BODY: GOVERNANCE.EMPTY_ASSIGN_BODY,
  PREVIEW_TITLE: GOVERNANCE.PREVIEW_TITLE,
  PREVIEW_LEAD: GOVERNANCE.PREVIEW_LEAD,
  PREVIEW_RUN_CTA: GOVERNANCE.PREVIEW_RUN_CTA,
  "PREVIEW_RESULT(name, matched)": GOVERNANCE.PREVIEW_RESULT("{name}", "{matched}"),
  PREVIEW_RESULT_DEFAULT: GOVERNANCE.PREVIEW_RESULT_DEFAULT,
  PREVIEW_RESULT_UNKNOWN: GOVERNANCE.PREVIEW_RESULT_UNKNOWN,
  PREVIEW_NOT_SAVED: GOVERNANCE.PREVIEW_NOT_SAVED,

  // ---- §7.4 ----
  GRANT_BOUND_TITLE: GOVERNANCE.GRANT_BOUND_TITLE,
  "DELETE_CONFIRM(name)": GOVERNANCE.DELETE_CONFIRM("{name}"),
  DELETE_RESTRICT_TITLE: GOVERNANCE.DELETE_RESTRICT_TITLE,
  OMISSION_TITLE: GOVERNANCE.OMISSION_TITLE,
  OMISSION_ACK: GOVERNANCE.OMISSION_ACK,

  // ---- §7.5 ----
  FETCH_FAILED_TITLE: GOVERNANCE.FETCH_FAILED_TITLE,
  FETCH_FAILED_BODY: GOVERNANCE.FETCH_FAILED_BODY,

  // ---- §7.6 ----
  "CEILING_PROFILE(name)": MEMBER.CEILING_PROFILE("{name}"),
  "GS_CHIP(name)": MEMBER.GS_CHIP("{name}"),
  "GS_BODY(name)": MEMBER.GS_BODY("{name}"),

  // ---- §7.7 ----
  "DENIED_TASK_MODE_EXEC(name)": MEMBER.DENIED_TASK_MODE_EXEC("{name}"),
  "DENIED_INTERACTIVE(name)": MEMBER.DENIED_INTERACTIVE("{name}"),
  "DENIED_SEED_AUTO_TOOLS(name)": MEMBER.DENIED_SEED_AUTO_TOOLS("{name}"),
  "DENIED_CODEX_HOLD(name)": MEMBER.DENIED_CODEX_HOLD("{name}"),
  "WARN_STORED_CLAMPED(policy, name)": MEMBER.WARN_STORED_CLAMPED("{policy}", "{name}"),
  "WARN_WORKSPACE_DENIED(host, name)": MEMBER.WARN_WORKSPACE_DENIED("{host}", "{name}"),
  "WARN_GRANT_DROPPED(name, kind, reason)": MEMBER.WARN_GRANT_DROPPED("{name}", "{kind}", "{reason}"),
  DENIED_STALE_GROUPS: MEMBER.DENIED_STALE_GROUPS,

  // ---- §7.8 ----
  HERO_SLOGAN: POSITIONING.HERO_SLOGAN,
  SETUP_SUBTITLE: POSITIONING.SETUP_SUBTITLE,

  // ---- §7.9 ----
  ROLE_SECURITY_ADMIN: DIRECTORY.ROLE_SECURITY_ADMIN,
  "SUGGEST_ROW(displayName, detail)": DIRECTORY.SUGGEST_ROW("{displayName}", "{detail}"),
  "GROUP_PICKED_CHIP(displayName)": DIRECTORY.GROUP_PICKED_CHIP("{displayName}"),
  GROUP_VALUE_NOTE: DIRECTORY.GROUP_VALUE_NOTE,
  SEARCHING: DIRECTORY.SEARCHING,
  MIN_CHARS_HINT: DIRECTORY.MIN_CHARS_HINT,
  NO_MATCHES: DIRECTORY.NO_MATCHES,
  LOOKUP_FAILED: DIRECTORY.LOOKUP_FAILED,
};

describe("governance-copy — §7.2-§7.9 parsed out of the prompt doc", () => {
  it("finds all 85 frozen keys in the doc", () => {
    expect(doc.size).toBe(85);
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    const covered = [...Object.keys(rendered), ...PLURALISED].sort();
    expect(covered).toEqual([...doc.keys()].sort());
  });

  it.each(Object.keys(rendered))("%s is byte-exact", (key) => {
    expect(rendered[key]).toBe(doc.get(key));
  });

  // The two inline-pluralised keys. The doc cell spells BOTH arms separated by
  // " / "; §7.2 pins the shape to PERM.ENFORCE_ON_BODY's inline ternary rather
  // than a second pluralisation helper, and §7.4 says DELETE_RESTRICT_BODY
  // uses the same one.
  it("ASSIGNED_COUNT renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("ASSIGNED_COUNT(n)")!.split(" / ");
    expect(GOVERNANCE.ASSIGNED_COUNT(1)).toBe(singular.replace("{n}", "1"));
    expect(GOVERNANCE.ASSIGNED_COUNT(0)).toBe(plural.replace("{n}", "0"));
    expect(GOVERNANCE.ASSIGNED_COUNT(4)).toBe(plural.replace("{n}", "4"));
  });

  it("DELETE_RESTRICT_BODY renders both arms of its doc cell", () => {
    const cell = doc.get("DELETE_RESTRICT_BODY(name, n)")!;
    const alternation = "{n} assignment / {n} assignments";
    expect(cell).toContain(alternation);
    expect(GOVERNANCE.DELETE_RESTRICT_BODY("Contractors", 1)).toBe(
      cell.replace(alternation, "1 assignment").replace(/\{name\}/g, "Contractors"),
    );
    expect(GOVERNANCE.DELETE_RESTRICT_BODY("Contractors", 3)).toBe(
      cell.replace(alternation, "3 assignments").replace(/\{name\}/g, "Contractors"),
    );
  });
});

// The one set of strings in this module that §7's TABLES do not freeze. §7.3's
// prose names all three verbatim as PREVIEW_RESULT's {matched} vocabulary, so
// they are canon — they are just canon the doc spelled in a sentence. Pinned
// here, and named as an addition, so the coverage test above staying at 85
// isn't read as "nothing else lives in this module".
describe("governance-copy — the §7.3 {matched} vocabulary (ADDITION)", () => {
  it("renders the three phrases §7.3's prose names", () => {
    // Whitespace-collapsed: the doc hard-wraps its prose, and "the everyone
    // assignment" straddles a line break in §7.3.
    const prose = readFileSync(DOC, "utf8").replace(/\s+/g, " ");
    for (const phrase of [GOVERNANCE.MATCHED_USER, GOVERNANCE.MATCHED_GROUP, GOVERNANCE.MATCHED_ALL]) {
      expect(prose).toContain(`"${phrase}"`);
    }
    expect(GOVERNANCE.PREVIEW_RESULT("Contractors", GOVERNANCE.MATCHED_GROUP)).toBe(
      'These claims resolve to "Contractors" — matched by a group assignment.',
    );
  });
});

describe("governance-copy — the reuse rules §7 spells out", () => {
  // §7.9: one string for the picker option, the table chip and the mapped-role
  // label, homed next to PEOPLE.ROLE_ADMIN / ROLE_MEMBER. Two homes for one
  // frozen label is how they drift — this pins the reference, not a copy.
  it("DIRECTORY.ROLE_SECURITY_ADMIN IS PEOPLE.ROLE_SECURITY_ADMIN", () => {
    expect(DIRECTORY.ROLE_SECURITY_ADMIN).toBe(PEOPLE.ROLE_SECURITY_ADMIN);
  });

  // §7.1: PRIORITY_NA is the same em dash PEOPLE.ADDED_CHART_NA renders but is
  // its OWN key, because the two mean different things and one must be able to
  // change without the other.
  it("PRIORITY_NA is its own key, equal to but not sourced from ADDED_CHART_NA", () => {
    expect(GOVERNANCE.PRIORITY_NA).toBe("—");
    expect(GOVERNANCE.PRIORITY_NA).toBe(PEOPLE.ADDED_CHART_NA);
  });

  // §7.4 / §7.1: the module deliberately carries NO copy of the strings the
  // server composes — the 409 delete refusal, the "invalid ceiling:" prefix
  // and its cause legs, and the five omission warnings. GRANT_BOUND_TITLE and
  // OMISSION_TITLE are HEADINGS over that wire text, which is why neither has
  // a _BODY twin here.
  it("carries no copy of the server-composed refusals and warnings", () => {
    const all = JSON.stringify(GOVERNANCE) + JSON.stringify(MEMBER);
    for (const serverOnly of [
      "invalid ceiling",
      "eligible grant",
      "this profile omits",
      "min_confinement_class",
      "allow_all_egress",
      "delete its assignments first",
    ]) {
      expect(all).not.toContain(serverOnly);
    }
    expect(GOVERNANCE).not.toHaveProperty("OMISSION_BODY");
    expect(GOVERNANCE).not.toHaveProperty("GRANT_BOUND_BODY");
  });
});
