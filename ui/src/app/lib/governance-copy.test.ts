/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import {
  AUTONOMY_BOUND,
  AUTONOMY_RAIL,
  autonomyBoundSentence,
  DIRECTORY,
  foldAutonomyRubric,
  GOVERNANCE,
  LIMITS_CHIP,
  MEMBER,
  POSITIONING,
  RUBRIC,
  RUN_LIMITS,
  runLimitUnit,
  runLimitsChip,
  setsRunLimits,
} from "./governance-copy";
import { PEOPLE } from "./people-access-copy";
import { AUTONOMY_RUBRIC_ROW_KEYS, type AutonomyRubricRowKey } from "./api/governance";

// The mock round's whole value is that it stays CHECKABLE, so this suite does
// not hand-retype a sample of the canon — it PARSES docs/design/
// governance-prompt.md §7.2-§7.9 back out of the doc and compares all 87 keys.
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
const PLURALISED = ["ASSIGNED_COUNT(n)", "DELETE_RESTRICT_BODY(name, n)", "LIMIT_QUOTA_LABEL(n)"];

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
  LIMIT_DRIVE_LABEL: GOVERNANCE.LIMIT_DRIVE_LABEL,
  LIMIT_DRIVE_HINT: GOVERNANCE.LIMIT_DRIVE_HINT,
  LIMIT_CONCURRENT_LABEL: GOVERNANCE.LIMIT_CONCURRENT_LABEL,
  LIMIT_CONCURRENT_HINT: GOVERNANCE.LIMIT_CONCURRENT_HINT,
  LIMIT_EPHEMERAL_LABEL: GOVERNANCE.LIMIT_EPHEMERAL_LABEL,
  LIMIT_EPHEMERAL_HINT: GOVERNANCE.LIMIT_EPHEMERAL_HINT,
  LIMIT_DRIVE_SIZE_LABEL: GOVERNANCE.LIMIT_DRIVE_SIZE_LABEL,
  LIMIT_DRIVE_SIZE_HINT: GOVERNANCE.LIMIT_DRIVE_SIZE_HINT,
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
  "DENIED_SEEDED_IMAGE(image)": MEMBER.DENIED_SEEDED_IMAGE("{image}"),
  DENIED_WORKSPACE_LLM_CRED: MEMBER.DENIED_WORKSPACE_LLM_CRED,

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
  it("finds all 96 frozen keys in the doc", () => {
    expect(doc.size).toBe(96);
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

  // R4/F032's addition, same inline-pluralisation shape §7.2 pins.
  it("LIMIT_QUOTA_LABEL renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("LIMIT_QUOTA_LABEL(n)")!.split(" / ");
    expect(GOVERNANCE.LIMIT_QUOTA_LABEL(1)).toBe(singular.replace("{n}", "1"));
    expect(GOVERNANCE.LIMIT_QUOTA_LABEL(3)).toBe(plural.replace("{n}", "3"));
    expect(GOVERNANCE.LIMIT_QUOTA_LABEL(0)).toBe(plural.replace("{n}", "0"));
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

// #93/#96 — the autonomy rubric. These strings live outside GOVERNANCE/MEMBER
// (never parsed from governance-prompt.md — the mock they were transcribed
// from is a scratchpad, not docs/), so they get their own direct pins rather
// than a doc-table comparison.
describe("RUBRIC — the profile editor's rubric section", () => {
  it("has one [label, why] row for every one of the nine AutonomyRubric fields, in the fixed order", () => {
    expect(Object.keys(RUBRIC.ROWS).sort()).toEqual([...AUTONOMY_RUBRIC_ROW_KEYS].sort());
    for (const k of AUTONOMY_RUBRIC_ROW_KEYS) {
      const [label, why] = RUBRIC.ROWS[k];
      expect(label.length).toBeGreaterThan(0);
      expect(why.length).toBeGreaterThan(0);
    }
  });

  it("SET_NOTE and EMPTY_NOTE match the mock round's frozen wording", () => {
    expect(RUBRIC.SET_NOTE(3, "Gated")).toBe("3 of 9 rows set a cap. The lowest is Gated.");
    expect(RUBRIC.EMPTY_NOTE).toBe("No row sets a cap, so this profile leaves autonomy exactly as it is today.");
  });
});

describe("foldAutonomyRubric", () => {
  it("nil/undefined rubric folds to no set rows and no lowest level", () => {
    expect(foldAutonomyRubric(undefined)).toEqual({ setKeys: [], lowest: null });
    expect(foldAutonomyRubric(null)).toEqual({ setKeys: [], lowest: null });
    expect(foldAutonomyRubric({})).toEqual({ setKeys: [], lowest: null });
  });

  it("the lowest level wins over every OTHER set row, whatever order they were set in", () => {
    expect(
      foldAutonomyRubric({ egress_open: "L3", secrets_powerful: "L0", confinement_cc1: "L2" }),
    ).toEqual({ setKeys: ["egress_open", "secrets_powerful", "confinement_cc1"], lowest: "L0" });
  });

  it("setKeys carries every set row, unset rows excluded", () => {
    expect(foldAutonomyRubric({ egress_open: "L1", egress_reviewed: undefined }).setKeys).toEqual(["egress_open"]);
  });
});

// RL-14 (0.8, #579) — the profile editor's Run limits section. Like RUBRIC
// above, these strings are transcribed from long-holds-packet.html, a
// scratchpad mock outside docs/, so they're pinned directly rather than
// parsed out of a doc.
describe("RUN_LIMITS — the profile editor's run-limits section (long-holds-design.md rev 4 §6)", () => {
  it("pins the seven field labels and their hints, byte-exact from the packet", () => {
    expect(RUN_LIMITS.MAX_END_LABEL).toBe("Longest a run can be set to last");
    expect(RUN_LIMITS.MAX_END_HINT).toBe("Measured from now. People extend before it ends. Leave blank for no limit.");
    expect(RUN_LIMITS.DEFAULT_END_LABEL).toBe("Default end");
    expect(RUN_LIMITS.ALLOW_NO_END_LABEL).toBe("Allow no end");
    expect(RUN_LIMITS.ALLOW_NO_END_HINT).toBe(
      "Runs keep going until someone ends them. They still pause when nobody is there, and keep their memory. Set a concurrent-run limit too.",
    );
    expect(RUN_LIMITS.MAX_WAIT_LABEL).toBe("Longest wait for a decision");
    expect(RUN_LIMITS.MAX_WAIT_HINT).toBe(
      "How long a run may keep a request open (a push, a tool call, a new site, a sign-in) before it's refused. Tool calls wait at most 27 hours.",
    );
    expect(RUN_LIMITS.DEFAULT_WAIT_LABEL).toBe("Default wait");
    expect(RUN_LIMITS.USER_CHANGES_LABEL).toBe("People may change their run's end and wait");
    expect(RUN_LIMITS.USER_CHANGES_HINT).toBe(
      "Anyone can extend within the limit. This also lets them shorten it, choose no end, and change the wait.",
    );
    expect(RUN_LIMITS.PAUSE_IDLE_LABEL).toBe("Pause a run nobody is using after");
    expect(RUN_LIMITS.PAUSE_IDLE_HINT).toBe(
      "No typing, no network traffic (downloads in progress count), and a quiet CPU. Leave blank to pause only runs waiting for a decision.",
    );
  });
});

describe("runLimitsChip — the packet's Summary chip line", () => {
  it("renders no chip for an undefined or empty run limits", () => {
    expect(runLimitsChip(undefined)).toBeNull();
    expect(runLimitsChip({})).toBeNull();
  });

  it("renders no chip for a zero/false run limits (0 is unlimited everywhere else in this module)", () => {
    expect(
      runLimitsChip({ max_end_ahead_sec: 0, max_wait_sec: 0, user_changes_limits: false }),
    ).toBeNull();
  });

  it("matches the packet's own example verbatim", () => {
    expect(
      runLimitsChip({ max_end_ahead_sec: 30 * 86400, max_wait_sec: 8 * 3600, user_changes_limits: true }),
    ).toBe("Ends within 30 days · waits up to 8 hours · people may change these");
  });

  it("includes only the clauses a field actually sets", () => {
    expect(runLimitsChip({ max_end_ahead_sec: 86400 })).toBe("Ends within 1 day");
    expect(runLimitsChip({ max_wait_sec: 1800 })).toBe("waits up to 30 minutes");
    expect(runLimitsChip({ user_changes_limits: true })).toBe("people may change these");
  });
});

describe("setsRunLimits — every one of the seven fields counts, not just the chip's three", () => {
  it("is false for an empty or all-zero run limits", () => {
    expect(setsRunLimits({})).toBe(false);
    expect(setsRunLimits({ max_end_ahead_sec: 0, default_end_sec: 0, allow_no_end: false, pause_idle_after_sec: 0 })).toBe(
      false,
    );
  });

  it("is true for each field set alone", () => {
    for (const l of [
      { max_end_ahead_sec: 86400 },
      { default_end_sec: 86400 },
      { allow_no_end: true },
      { max_wait_sec: 3600 },
      { default_wait_sec: 3600 },
      { user_changes_limits: true },
      { pause_idle_after_sec: 1800 },
    ]) {
      expect(setsRunLimits(l), JSON.stringify(l)).toBe(true);
    }
  });
});

describe("runLimitUnit — the largest unit a value is a whole number of", () => {
  it("picks days, hours, minutes, then seconds", () => {
    expect(runLimitUnit(2 * 86400).many).toBe("days");
    expect(runLimitUnit(36 * 3600).many).toBe("hours");
    expect(runLimitUnit(1800).many).toBe("minutes");
    expect(runLimitUnit(20).many).toBe("seconds");
  });
});

describe("LIMITS_CHIP.AUTONOMY — ruling 2 (#96 review)", () => {
  it("names the strictest cap on the chip face itself, not merely that a rubric exists", () => {
    expect(LIMITS_CHIP.AUTONOMY("Attended")).toBe("Autonomy: Attended at the strictest");
  });
});

describe("AUTONOMY_BOUND / autonomyBoundSentence — ruling 1 (#96 review)", () => {
  it("every row has its own frozen one-cause sentence, in the 'Bound by...' shape", () => {
    for (const k of AUTONOMY_RUBRIC_ROW_KEYS) {
      expect(AUTONOMY_BOUND[k]).toMatch(/^Bound by this run's /);
      expect(AUTONOMY_BOUND[k].endsWith(".")).toBe(true);
    }
    expect(AUTONOMY_BOUND.secrets_powerful).toBe("Bound by this run's secrets: it carries a credential that can write.");
    expect(AUTONOMY_BOUND.confinement_cc1).toBe("Bound by this run's barrier: Fence, confinement class CC1.");
  });

  it("a single cause renders the SAME sentence AUTONOMY_BOUND carries, unchanged", () => {
    for (const k of AUTONOMY_RUBRIC_ROW_KEYS) {
      expect(autonomyBoundSentence([k])).toBe(AUTONOMY_BOUND[k]);
    }
  });

  it("no bound_by at all falls back to the no-cap sentence rather than an empty claim", () => {
    expect(autonomyBoundSentence([])).toBe(AUTONOMY_RAIL.NO_CAP);
  });

  // Ruling 1's own example (governance.spec.ts issue #96, the review comment):
  // "Bound by this run's secrets and its barrier: it carries a credential
  // that can write, behind a Fence (confinement class CC1)." is the mock's
  // scaffold illustration, not a frozen sentence (it lives in the mock's
  // .qblock, which the mock's own header marks as review material that ships
  // nowhere) — this pins the SHAPE the ruling requires instead: every tied
  // cause named, never just the first.
  it("a two-way tie names BOTH causes, not just the first", () => {
    const s = autonomyBoundSentence(["secrets_powerful", "confinement_cc1"]);
    expect(s).toContain("secrets");
    expect(s).toContain("barrier");
    expect(s).toContain("it carries a credential that can write");
    expect(s).toContain("Fence, confinement class CC1");
    // NOT the single-cause sentence for either cause alone — this is the
    // regression ruling 1 exists to prevent (bound_by[0] only).
    expect(s).not.toBe(AUTONOMY_BOUND.secrets_powerful);
    expect(s).not.toBe(AUTONOMY_BOUND.confinement_cc1);
  });

  it("a three-way tie names all three causes", () => {
    const causes: AutonomyRubricRowKey[] = ["egress_open", "secrets_none", "confinement_cc3"];
    const s = autonomyBoundSentence(causes);
    expect(s).toContain("network reach");
    expect(s).toContain("secrets");
    expect(s).toContain("barrier");
    expect(s).toContain("it can reach hosts beyond the baseline");
    expect(s).toContain("it carries none");
    expect(s).toContain("Vault, confinement class CC3");
  });

  // Finding 4 (#339 review): confinement's own detail carries a comma
  // ("Fence, confinement class CC1"), so joining a three-way tie's details
  // with plain ", " used to read as FOUR comma-separated fragments instead
  // of three. Semicolons between details keep the three causes distinct.
  it("a three-way tie stays unambiguous when one detail carries its own comma", () => {
    const causes: AutonomyRubricRowKey[] = ["egress_open", "secrets_powerful", "confinement_cc1"];
    const s = autonomyBoundSentence(causes);
    expect(s).toContain("network reach");
    expect(s).toContain("secrets");
    expect(s).toContain("barrier");
    expect(s).toContain("it can reach hosts beyond the baseline");
    expect(s).toContain("it carries a credential that can write");
    expect(s).toContain("Fence, confinement class CC1");
    // Exactly three semicolon-delimited details — the comma inside
    // confinement's own detail never reads as a fourth item boundary.
    const detailClause = s.slice(s.indexOf(": ") + 2, -1);
    expect(detailClause.split("; ")).toHaveLength(3);
  });
});
