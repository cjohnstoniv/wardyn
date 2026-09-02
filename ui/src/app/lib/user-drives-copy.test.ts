/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import * as GovernanceCopy from "./governance-copy";
import { MEMBER } from "./governance-copy";
import * as UserDrivesCopy from "./user-drives-copy";
import { DRIVE_MEMBER, DRIVE_RUN, DRIVES } from "./user-drives-copy";

// The mock round's whole value is that it stays CHECKABLE, so this suite does
// not hand-retype a sample of the canon — it PARSES docs/design/
// user-drives-prompt.md §7.2-§7.8 back out of the doc and compares all 140
// keys. A swapped hyphen, a dropped ellipsis, a reworded clause, a new doc
// row or a deleted one all fail here rather than shipping.
//
// Two normalisations, both of them documented rules rather than fudges:
//   - BACKTICKS ARE STRIPPED from the doc cell. §7's header note makes mono a
//     DISPLAY concern applied by the consuming component (the
//     governance-copy.ts precedent); the frozen string itself is plain text.
//     Unlike governance-prompt.md, this doc's §7.2-§7.8 DOES use backticks
//     as content (the mount target, env vars, `. _ -`), so unmono() does
//     real work here — every DRIVES / DRIVE_MEMBER string below is written
//     with the backtick-wrapped substring spelled as plain text.
//   - A PARAMETERIZED key is called with its own placeholder text, so
//     EDITOR_TITLE_EDIT("{name}") must reproduce the doc's `Edit "{name}"`
//     character for character. The four inline-pluralised keys and the two
//     numeric size helpers can't be checked that way and get their own tests
//     below (§5 #9, #10).

// process.cwd() is the vitest root — ui/ — for every entry point that runs
// this suite (`pnpm vitest run`, `pnpm test`, make ci). import.meta.url is not
// a file: URL under the jsdom environment, so it can't resolve this.
const DOC = resolve(process.cwd(), "../docs/design/user-drives-prompt.md");

const unmono = (s: string) => s.replace(/`/g, "");

/** key -> frozen string, for every row of §7.2-§7.8's tables. */
function parseFrozenTables(): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      // §7.2-§7.8 only: §7.1 is the reused-canon table (strings that live in
      // permissions-copy.ts / people-access-copy.ts / governance-copy.ts /
      // the server) and its second table (server-composed, no Key column),
      // neither of which are keys this module freezes. There is no §7.9 in
      // this doc.
      inSection = /^### 7\.[2-8]\b/.test(line);
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

// The four keys whose doc cell carries an "A / B" pluralisation alternation
// rather than a single renderable string (§5 #9) — checked in their own test.
const PLURALISED = ["ALLOCATED_COUNT(n)", "DELETE_RESTRICT_BODY(name, n)", "CARD_DRIVES(n)", "CARD_ALLOCATIONS(n)"];

// The two size helpers take a NUMBER, not a string placeholder, so they
// can't go through the "{n}" substitution path either (§5 #10) — checked in
// their own test against the doc cell with {n} substituted.
const SIZE_HELPERS = ["SIZE_MIB(n)", "SIZE_GIB(n)"];

// Every other key, rendered from the module exactly as the doc spells it
// (backticks stripped, per the mono rule above).
const rendered: Record<string, string> = {
  // ---- §7.2 ----
  TITLE: DRIVES.TITLE,
  LEAD: DRIVES.LEAD,
  DRIVES_TITLE: DRIVES.DRIVES_TITLE,
  DRIVES_LEAD: DRIVES.DRIVES_LEAD,
  COL_NAME: DRIVES.COL_NAME,
  COL_BACKEND: DRIVES.COL_BACKEND,
  COL_SIZE: DRIVES.COL_SIZE,
  COL_MODE: DRIVES.COL_MODE,
  COL_RECLAIM: DRIVES.COL_RECLAIM,
  COL_ALLOCATED: DRIVES.COL_ALLOCATED,
  NEW_CTA: DRIVES.NEW_CTA,
  EDIT: DRIVES.EDIT,
  DELETE: DRIVES.DELETE,
  KIND_MANAGED: DRIVES.KIND_MANAGED,
  KIND_SHARE: DRIVES.KIND_SHARE,
  ALLOCATED_NONE: DRIVES.ALLOCATED_NONE,
  MODE_RO: DRIVES.MODE_RO,
  MODE_RW: DRIVES.MODE_RW,
  MODE_RO_INLINE: DRIVES.MODE_RO_INLINE,
  MODE_RW_INLINE: DRIVES.MODE_RW_INLINE,
  SIZE_NONE: DRIVES.SIZE_NONE,
  EMPTY_TITLE: DRIVES.EMPTY_TITLE,
  EMPTY_BODY: DRIVES.EMPTY_BODY,
  EDITOR_TITLE_NEW: DRIVES.EDITOR_TITLE_NEW,
  "EDITOR_TITLE_EDIT(name)": DRIVES.EDITOR_TITLE_EDIT("{name}"),
  FIELD_NAME: DRIVES.FIELD_NAME,
  NAME_HINT: DRIVES.NAME_HINT,
  FIELD_BACKEND: DRIVES.FIELD_BACKEND,
  BACKEND_HINT: DRIVES.BACKEND_HINT,
  BACKEND_DOCKER_VOLUME: DRIVES.BACKEND_DOCKER_VOLUME,
  BACKEND_HOST_PATH: DRIVES.BACKEND_HOST_PATH,
  BACKEND_K8S_PVC: DRIVES.BACKEND_K8S_PVC,
  BACKEND_K8S_PVC_STATIC: DRIVES.BACKEND_K8S_PVC_STATIC,
  BACKEND_UNAVAILABLE_DOCKER_ROOTS: DRIVES.BACKEND_UNAVAILABLE_DOCKER_ROOTS,
  FIELD_HOST_ROOT: DRIVES.FIELD_HOST_ROOT,
  HOST_ROOT_HINT: DRIVES.HOST_ROOT_HINT,
  FIELD_STORAGE_CLASS: DRIVES.FIELD_STORAGE_CLASS,
  STORAGE_CLASS_HINT: DRIVES.STORAGE_CLASS_HINT,
  FIELD_HOME: DRIVES.FIELD_HOME,
  HOME_HINT: DRIVES.HOME_HINT,
  HOME_HASH: DRIVES.HOME_HASH,
  HOME_HASH_HINT: DRIVES.HOME_HASH_HINT,
  HOME_SUB: DRIVES.HOME_SUB,
  HOME_SUB_HINT: DRIVES.HOME_SUB_HINT,
  HOME_EMAIL_LOCAL: DRIVES.HOME_EMAIL_LOCAL,
  HOME_EMAIL_LOCAL_HINT: DRIVES.HOME_EMAIL_LOCAL_HINT,
  HOME_RULE: DRIVES.HOME_RULE,
  FIELD_SIZE: DRIVES.FIELD_SIZE,
  SIZE_HINT: DRIVES.SIZE_HINT,
  SIZE_HINT_REQUIRED: DRIVES.SIZE_HINT_REQUIRED,
  FIELD_WRITABLE: DRIVES.FIELD_WRITABLE,
  WRITABLE_HINT: DRIVES.WRITABLE_HINT,
  FIELD_RECLAIM: DRIVES.FIELD_RECLAIM,
  RECLAIM_RETAIN: DRIVES.RECLAIM_RETAIN,
  RECLAIM_DELETE: DRIVES.RECLAIM_DELETE,
  RECLAIM_HINT: DRIVES.RECLAIM_HINT,
  SAVE_CTA: DRIVES.SAVE_CTA,
  SAVE_ERROR: DRIVES.SAVE_ERROR,
  SAVE_REFUSED_TITLE: DRIVES.SAVE_REFUSED_TITLE,
  ENFORCEMENT_FILESYSTEM: DRIVES.ENFORCEMENT_FILESYSTEM,
  ENFORCEMENT_REQUEST: DRIVES.ENFORCEMENT_REQUEST,
  ENFORCEMENT_EXTERNAL: DRIVES.ENFORCEMENT_EXTERNAL,
  ENFORCEMENT_NONE: DRIVES.ENFORCEMENT_NONE,
  HONESTY: DRIVES.HONESTY,

  // ---- §7.3 ----
  ALLOC_TITLE: DRIVES.ALLOC_TITLE,
  ALLOC_LEAD: DRIVES.ALLOC_LEAD,
  PRECEDENCE: DRIVES.PRECEDENCE,
  EFFECT_NOTE: DRIVES.EFFECT_NOTE,
  SIGNIN_NOTE: DRIVES.SIGNIN_NOTE,
  ADD_TITLE: DRIVES.ADD_TITLE,
  ADD_CTA: DRIVES.ADD_CTA,
  FIELD_DRIVE: DRIVES.FIELD_DRIVE,
  DRIVE_PLACEHOLDER: DRIVES.DRIVE_PLACEHOLDER,
  FIELD_SIZE_OVERRIDE: DRIVES.FIELD_SIZE_OVERRIDE,
  SIZE_OVERRIDE_HINT: DRIVES.SIZE_OVERRIDE_HINT,
  FIELD_WRITABLE_OVERRIDE: DRIVES.FIELD_WRITABLE_OVERRIDE,
  WRITABLE_OVERRIDE_INHERIT: DRIVES.WRITABLE_OVERRIDE_INHERIT,
  WRITABLE_OVERRIDE_HINT: DRIVES.WRITABLE_OVERRIDE_HINT,
  FIELD_HOME_OVERRIDE: DRIVES.FIELD_HOME_OVERRIDE,
  HOME_OVERRIDE_HINT: DRIVES.HOME_OVERRIDE_HINT,
  HOME_OVERRIDE_NA: DRIVES.HOME_OVERRIDE_NA,
  FIELD_ENABLED: DRIVES.FIELD_ENABLED,
  ENABLED_HINT: DRIVES.ENABLED_HINT,
  COL_DRIVE: DRIVES.COL_DRIVE,
  COL_OVERRIDES: DRIVES.COL_OVERRIDES,
  OVERRIDES_NONE: DRIVES.OVERRIDES_NONE,
  "OVERRIDE_SIZE(size)": DRIVES.OVERRIDE_SIZE("{size}"),
  "OVERRIDE_HOME(name)": DRIVES.OVERRIDE_HOME("{name}"),
  PAUSED_CHIP: DRIVES.PAUSED_CHIP,
  ALLOC_REPLACED: DRIVES.ALLOC_REPLACED,
  "REMOVE_CONFIRM(who, name)": DRIVES.REMOVE_CONFIRM("{who}", "{name}"),
  EMPTY_ALLOC_TITLE: DRIVES.EMPTY_ALLOC_TITLE,
  EMPTY_ALLOC_BODY: DRIVES.EMPTY_ALLOC_BODY,
  PREVIEW_TITLE: DRIVES.PREVIEW_TITLE,
  PREVIEW_LEAD: DRIVES.PREVIEW_LEAD,
  PREVIEW_CTA: DRIVES.PREVIEW_CTA,
  PREVIEW_NONE: DRIVES.PREVIEW_NONE,
  "PREVIEW_RESULT(drive, tier)": DRIVES.PREVIEW_RESULT("{drive}", "{tier}"),
  PREVIEW_TIER_USER: DRIVES.PREVIEW_TIER_USER,
  PREVIEW_TIER_GROUP: DRIVES.PREVIEW_TIER_GROUP,
  PREVIEW_TIER_ALL: DRIVES.PREVIEW_TIER_ALL,
  PREVIEW_OBJECT_LABEL: DRIVES.PREVIEW_OBJECT_LABEL,
  PREVIEW_OBJECT_HINT: DRIVES.PREVIEW_OBJECT_HINT,
  PREVIEW_ENFORCEMENT_LABEL: DRIVES.PREVIEW_ENFORCEMENT_LABEL,

  // ---- §7.4 ----
  "DELETE_CONFIRM(name)": DRIVES.DELETE_CONFIRM("{name}"),
  DELETE_RESTRICT_TITLE: DRIVES.DELETE_RESTRICT_TITLE,
  FETCH_FAILED_TITLE: DRIVES.FETCH_FAILED_TITLE,
  FETCH_FAILED_BODY: DRIVES.FETCH_FAILED_BODY,

  // ---- §7.5 ----
  CARD_LEAD: DRIVES.CARD_LEAD,
  CARD_EMPTY: DRIVES.CARD_EMPTY,
  "CARD_SUMMARY(drives, allocations)": DRIVES.CARD_SUMMARY("{drives}", "{allocations}"),
  CARD_OPEN: DRIVES.CARD_OPEN,

  // ---- §7.6 ----
  NR_CHECKBOX: DRIVE_MEMBER.NR_CHECKBOX,
  "NR_HINT(name, size, mode)": DRIVE_MEMBER.NR_HINT("{name}", "{size}", "{mode}"),
  "NR_HINT_NOSIZE(name, mode)": DRIVE_MEMBER.NR_HINT_NOSIZE("{name}", "{mode}"),
  NR_RW_NOTE: DRIVE_MEMBER.NR_RW_NOTE,
  NR_RO_NOTE: DRIVE_MEMBER.NR_RO_NOTE,
  NR_READONLY_TOGGLE: DRIVE_MEMBER.NR_READONLY_TOGGLE,
  NR_PAUSED: DRIVE_MEMBER.NR_PAUSED,
  "NR_DENIED(profile)": DRIVE_MEMBER.NR_DENIED("{profile}"),
  "GS_DRIVE_CHIP(name, size, mode)": DRIVE_MEMBER.GS_DRIVE_CHIP("{name}", "{size}", "{mode}"),
  "GS_DRIVE_CHIP_NOSIZE(name, mode)": DRIVE_MEMBER.GS_DRIVE_CHIP_NOSIZE("{name}", "{mode}"),
  "GS_DRIVE_CHIP_PAUSED(name)": DRIVE_MEMBER.GS_DRIVE_CHIP_PAUSED("{name}"),
  GS_DRIVE_BODY: DRIVE_MEMBER.GS_DRIVE_BODY,

  // ---- §7.7 ----
  "DENIED_DRIVE(name)": DRIVE_MEMBER.DENIED_DRIVE("{name}"),
  REFUSED_NO_GRANT: DRIVE_MEMBER.REFUSED_NO_GRANT,
  REFUSED_PAUSED: DRIVE_MEMBER.REFUSED_PAUSED,
  "REFUSED_HOME_INVALID(claim)": DRIVE_MEMBER.REFUSED_HOME_INVALID("{claim}"),
  "REFUSED_HOME_MISSING(name)": DRIVE_MEMBER.REFUSED_HOME_MISSING("{name}"),
  REFUSED_WRITABLE: DRIVE_MEMBER.REFUSED_WRITABLE,
  "REFUSED_BACKEND(reason)": DRIVE_MEMBER.REFUSED_BACKEND("{reason}"),
  REFUSED_TARGET_RESERVED: DRIVE_MEMBER.REFUSED_TARGET_RESERVED,

  // ---- §7.8 ----
  RAIL_LABEL: DRIVE_RUN.RAIL_LABEL,
  "RAIL_VALUE(name, mode)": DRIVE_RUN.RAIL_VALUE("{name}", "{mode}"),
};

describe("user-drives-copy — §7.2-§7.8 parsed out of the prompt doc", () => {
  it("finds all 140 frozen keys in the doc", () => {
    expect(doc.size).toBe(140);
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    const covered = [...Object.keys(rendered), ...PLURALISED, ...SIZE_HELPERS].sort();
    expect(covered).toEqual([...doc.keys()].sort());
  });

  it.each(Object.keys(rendered))("%s is byte-exact", (key) => {
    expect(rendered[key]).toBe(doc.get(key));
  });

  // The four inline-pluralised keys (§5 #9). The doc cell spells BOTH arms
  // separated by " / "; §7.2 pins the shape to PERM.ENFORCE_ON_BODY's inline
  // ternary rather than a second pluralisation helper.
  it("ALLOCATED_COUNT renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("ALLOCATED_COUNT(n)")!.split(" / ");
    expect(DRIVES.ALLOCATED_COUNT(1)).toBe(singular.replace("{n}", "1"));
    expect(DRIVES.ALLOCATED_COUNT(0)).toBe(plural.replace("{n}", "0"));
    expect(DRIVES.ALLOCATED_COUNT(4)).toBe(plural.replace("{n}", "4"));
  });

  it("DELETE_RESTRICT_BODY renders both arms of its doc cell", () => {
    const cell = doc.get("DELETE_RESTRICT_BODY(name, n)")!;
    const alternation = "{n} subject / {n} subjects";
    expect(cell).toContain(alternation);
    expect(DRIVES.DELETE_RESTRICT_BODY("Contractors", 1)).toBe(
      cell.replace(alternation, "1 subject").replace(/\{name\}/g, "Contractors"),
    );
    expect(DRIVES.DELETE_RESTRICT_BODY("Contractors", 3)).toBe(
      cell.replace(alternation, "3 subjects").replace(/\{name\}/g, "Contractors"),
    );
  });

  it("CARD_DRIVES renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("CARD_DRIVES(n)")!.split(" / ");
    expect(DRIVES.CARD_DRIVES(1)).toBe(singular.replace("{n}", "1"));
    expect(DRIVES.CARD_DRIVES(0)).toBe(plural.replace("{n}", "0"));
    expect(DRIVES.CARD_DRIVES(2)).toBe(plural.replace("{n}", "2"));
  });

  it("CARD_ALLOCATIONS renders both arms of its doc cell", () => {
    const [singular, plural] = doc.get("CARD_ALLOCATIONS(n)")!.split(" / ");
    expect(DRIVES.CARD_ALLOCATIONS(1)).toBe(singular.replace("{n}", "1"));
    expect(DRIVES.CARD_ALLOCATIONS(0)).toBe(plural.replace("{n}", "0"));
    expect(DRIVES.CARD_ALLOCATIONS(5)).toBe(plural.replace("{n}", "5"));
  });

  // The two size helpers (§5 #10): they take a NUMBER, so they are checked
  // against the doc cell with {n} substituted rather than through the
  // placeholder-substitution path `rendered` uses for string args.
  it("SIZE_MIB(8) matches its doc cell with {n} substituted", () => {
    expect(DRIVES.SIZE_MIB(8)).toBe(doc.get("SIZE_MIB(n)")!.replace("{n}", "8"));
  });

  it("SIZE_GIB(2) matches its doc cell with {n} substituted", () => {
    expect(DRIVES.SIZE_GIB(2)).toBe(doc.get("SIZE_GIB(n)")!.replace("{n}", "2"));
  });
});

describe("user-drives-copy — the reuse and no-shadow rules §5 spells out", () => {
  // §5 #11: "The drives module exports no name governance-copy.ts already
  // exports." GOVERNANCE, MEMBER, POSITIONING and DIRECTORY are
  // governance-copy.ts's own frozen namespaces; PERM / ACCESS_STATE / PEOPLE
  // / PREVIEW are passthrough re-exports of permissions-copy.ts /
  // people-access-copy.ts and are identical whichever module re-exports
  // them, so a name shared THERE isn't a shadow — this checks the four
  // namespaces governance-copy.ts itself originates.
  it("DRIVES / DRIVE_MEMBER / DRIVE_RUN collide with none of governance-copy.ts's own namespace names", () => {
    const governanceOwn = ["GOVERNANCE", "MEMBER", "POSITIONING", "DIRECTORY"];
    for (const name of Object.keys(UserDrivesCopy)) {
      if (governanceOwn.includes(name)) {
        throw new Error(`user-drives-copy.ts exports "${name}", which governance-copy.ts already exports`);
      }
    }
    expect(["DRIVES", "DRIVE_MEMBER", "DRIVE_RUN"].every((n) => Object.keys(UserDrivesCopy).includes(n))).toBe(true);
  });

  // The doc's own illustration for #11: MEMBER.GS_CHIP / GS_BODY (governance)
  // and DRIVE_MEMBER.GS_DRIVE_* render on the SAME Getting Started screen, so
  // a name collision between the two objects' OWN keys is the concrete risk
  // the rule guards against — checked directly, rather than assuming the
  // namespace-name check above covers it.
  it("DRIVE_MEMBER shares no key with governance-copy.ts's MEMBER", () => {
    const overlap = Object.keys(DRIVE_MEMBER).filter((k) => Object.keys(MEMBER).includes(k));
    expect(overlap).toEqual([]);
  });

  it("DRIVE_RUN shares no key with any governance-copy.ts namespace", () => {
    const all = new Set([
      ...Object.keys(GovernanceCopy.GOVERNANCE),
      ...Object.keys(GovernanceCopy.MEMBER),
      ...Object.keys(GovernanceCopy.POSITIONING),
      ...Object.keys(GovernanceCopy.DIRECTORY),
    ]);
    const overlap = Object.keys(DRIVE_RUN).filter((k) => all.has(k));
    expect(overlap).toEqual([]);
  });

  // DRIVES and GOVERNANCE are two separate admin screens that deliberately
  // freeze the SAME short field names for the same concept (TITLE, EDIT,
  // DELETE, COL_NAME, NEW_CTA, EMPTY_TITLE/_BODY, SAVE_ERROR,
  // FETCH_FAILED_TITLE/_BODY, EDITOR_TITLE_NEW/_EDIT, FIELD_NAME, NAME_HINT,
  // ADD_TITLE/_CTA, PRECEDENCE, EFFECT_NOTE, SIGNIN_NOTE, PREVIEW_TITLE/_LEAD
  // /_RESULT, DELETE_CONFIRM, DELETE_RESTRICT_TITLE/_BODY — both tables
  // freeze these verbatim, so §5 #11's "no name" guarantee is the NAMESPACE
  // name (checked above), not every inner key: DRIVES and GOVERNANCE are
  // never imported at one call site the way DRIVE_MEMBER and MEMBER are.
  it("documents that DRIVES intentionally reuses GOVERNANCE's inner key names", () => {
    const overlap = Object.keys(DRIVES).filter((k) => Object.keys(GovernanceCopy.GOVERNANCE).includes(k));
    expect(overlap.length).toBeGreaterThan(0);
  });

  // §7.1 / Hard canon #4: the module deliberately carries NO copy of the
  // ADMIN-facing strings the server composes (validateUserDrive,
  // handleDeleteUserDrive, validateUserDriveGrant) — the member's own §7.7
  // doors are frozen as canon keys instead (checked for byte-exactness
  // above), but these seven admin refusals are rendered from the wire only.
  it("carries no copy of the admin-facing server-composed refusals", () => {
    const all = JSON.stringify(DRIVES) + JSON.stringify(DRIVE_MEMBER) + JSON.stringify(DRIVE_RUN);
    for (const serverOnly of [
      "is not inside WARDYN_USER_DRIVE_HOST_ROOTS",
      "is under a denied prefix",
      "cannot be mounted by this deployment's runner",
      "is not allowed on a share backend",
      "must be above 0 for a k8s_pvc drive",
      "remove its allocations first",
      "a group cannot share one directory",
    ]) {
      expect(all).not.toContain(serverOnly);
    }
  });
});
