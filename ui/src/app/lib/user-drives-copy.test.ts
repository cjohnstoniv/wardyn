/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readdirSync, readFileSync } from "node:fs";
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
      // this doc. §7.1's second table is not unguarded, though — it is checked
      // against the Go source at the bottom of this file, where the truth is
      // the emitting literal rather than a key in this module.
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

// Every other key is rendered FROM THE DOC's own key cell rather than from a
// hand-typed table: `EDITOR_TITLE_EDIT(name)` is split into the module symbol
// and its argument names, and the value is called with "{name}" so it must
// reproduce the doc cell character for character. That kills the third copy of
// the key list (doc row, module, and — until now — this file) and is strictly
// stronger than the map it replaces: a module key with NO doc row now fails
// too, which a hand-written map by construction could never notice.
const NAMESPACES: Record<string, unknown>[] = [DRIVES, DRIVE_MEMBER, DRIVE_RUN];

/** `EDITOR_TITLE_EDIT(name)` -> ["EDITOR_TITLE_EDIT", ["name"]]. */
function splitKey(docKey: string): [string, string[]] {
  const m = /^([A-Z0-9_]+)\((.*)\)$/.exec(docKey);
  return m ? [m[1], m[2].split(",").map((a) => a.trim())] : [docKey, []];
}

// ONE lookup across the three namespaces is safe because none of their keys
// collide (118 / 20 / 2); the completeness test below is what keeps that true.
function render(docKey: string): string {
  const [name, args] = splitKey(docKey);
  const ns = NAMESPACES.find((n) => name in n);
  if (!ns) throw new Error(`${docKey}: no such key in DRIVES / DRIVE_MEMBER / DRIVE_RUN`);
  const value = ns[name];
  return typeof value === "function" ? (value as (...a: string[]) => string)(...args.map((a) => `{${a}}`)) : String(value);
}

// The six keys that cannot go through the placeholder path get their own tests
// below (§5 #9, #10).
const EXCLUDED = [...PLURALISED, ...SIZE_HELPERS];
const RENDERABLE = [...doc.keys()].filter((k) => !EXCLUDED.includes(k));

describe("user-drives-copy — §7.2-§7.8 parsed out of the prompt doc", () => {
  it("finds all 140 frozen keys in the doc", () => {
    expect(doc.size).toBe(140);
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    // BOTH directions, which is what the hand-typed map could only half do:
    // every doc row resolves to a module symbol, and every module symbol has a
    // doc row. A key added to the module and forgotten in §7 fails here.
    const docNames = [...doc.keys()].map((k) => splitKey(k)[0]).sort();
    const moduleNames = NAMESPACES.flatMap((n) => Object.keys(n)).sort();
    expect(moduleNames).toEqual(docNames);
  });

  it.each(RENDERABLE)("%s is byte-exact", (key) => {
    expect(render(key)).toBe(doc.get(key));
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

// ─── §7.1's second table: the strings the SERVER composes ───────────────────
//
// The table above freezes what the drives MODULE renders. This one freezes
// what the module deliberately does NOT carry — the admin-facing refusals the
// Go side composes and the console renders verbatim off the wire — and until
// now nothing checked it: parseFrozenTables() skips §7.1 by design (it has no
// Key column), so the one table an implementer is told to render without a key
// was the only one that could drift, and it had. It named a refusal that
// existed nowhere in the tree, gave two host_root rows a 400 the code answers
// 422 for, mis-cased two emitters, and omitted four shipped strings.
//
// So the doc is checked against the CODE here, in the direction the campaign
// requires: the Go literal is the truth for a server-composed string, and a
// row the code cannot emit fails.
//
// ponytail: the check is ONE-WAY — every doc row must exist in Go, but a NEW
// server-composed refusal added without a doc row still passes. Closing that
// needs a marker on the Go side saying "this literal is §7.1 canon" (there is
// no other way to tell a frozen refusal from the dozens of ordinary
// `fmt.Errorf("field: invalid")` shapes in the same files), which is a Go
// change this doc fix does not carry. The four omissions this round were found
// by reading, and are now rows.

const GO_DIRS = ["internal/api", "internal/types", "internal/runner"];

/**
 * Every interpreted string literal in the drives-bearing Go packages, with Go's
 * source-level concatenation (`"a " +\n "b"`) already joined so a wrapped
 * message reads as the one string it is at runtime.
 */
function goStringLiterals(): string[] {
  const out: string[] = [];
  for (const dir of GO_DIRS) {
    const abs = resolve(process.cwd(), "..", dir);
    for (const name of readdirSync(abs)) {
      if (!name.endsWith(".go") || name.endsWith("_test.go")) continue;
      const joined = readFileSync(resolve(abs, name), "utf8").replace(/"\s*\+\s*\n?\s*"/g, "");
      for (const m of joined.matchAll(/"((?:[^"\\\n]|\\.)*)"/g)) {
        out.push(m[1].replace(/\\(.)/g, (_, c) => (c === "n" ? "\n" : c === "t" ? "\t" : c)));
      }
    }
  }
  return out;
}

/** Every `func Name(` / `func (recv) Name(` declared in those same packages. */
function goFuncNames(): Set<string> {
  const names = new Set<string>();
  for (const dir of GO_DIRS) {
    const abs = resolve(process.cwd(), "..", dir);
    for (const name of readdirSync(abs)) {
      if (!name.endsWith(".go") || name.endsWith("_test.go")) continue;
      for (const m of readFileSync(resolve(abs, name), "utf8").matchAll(/^func (?:\([^)]*\) )?([A-Za-z_]\w*)\(/gm)) {
        names.add(m[1]);
      }
    }
  }
  return names;
}

/**
 * The write boundary's envelope, stripped from BOTH sides: `decodeUserDrive-
 * Request` and `handleUpsertUserDriveGrant` prefix the validator's error, and
 * `driveHostRootNesting` bakes the same prefix into its own literal. The table
 * freezes the refusal, not the envelope, so neither side carries it.
 */
const unenvelope = (s: string) => s.replace(/^(?:invalid drive|invalid allocation): /, "");

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** A Go literal cut at its format verbs: the runs of text the output is fixed at. */
const goFixedRuns = (lit: string) => unenvelope(lit).split(/%[#+\- 0]*\d*(?:\.\d+)?[a-zA-Z]/);

/**
 * A Go literal as the pattern its runtime output must fit: every format verb
 * becomes a wildcard, everything else is fixed. That is what lets one row cover
 * both spellings the doc legitimately uses for a verb — `"{path}"` for a value
 * only the request knows, and the constant itself (`k8s_pvc`, `email_local`)
 * where the verb is always given the same one.
 *
 * THE FIXED FLOOR IS LOAD-BEARING, not a tuning knob: without it the bare `"%s"`
 * that exists in these packages compiles to `^.*?$`, matches every row, and the
 * whole check passes vacuously. A frozen refusal is a sentence; 30 characters of
 * it are fixed by construction.
 */
const MIN_FIXED = 30;
const goLiteralPattern = (lit: string) => new RegExp(`^${goFixedRuns(lit).map(escapeRe).join(".*?")}$`);
const goLiteralIsASentence = (lit: string) => goFixedRuns(lit).join("").length >= MIN_FIXED;

/** The fixed runs long enough to identify the refusal they came from. */
const goLiteralChunks = (lit: string) =>
  goFixedRuns(lit)
    .map((c) => c.trim())
    .filter((c) => c.length >= 10);

type ServerRow = { source: string; emitter: string; text: string; status: number };

/** §7.1's second table — the one with a `Source` column instead of a `Key` one. */
function parseServerComposedTable(): ServerRow[] {
  const rows: ServerRow[] = [];
  let started = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (!line.startsWith("|")) {
      if (started) break; // the table ended
      continue;
    }
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Source") {
      started = true;
      continue;
    }
    if (!started || /^:?-+:?$/.test(cells[0])) continue;
    const status = /\((?:[^()]*,\s*)?(\d{3})\)/.exec(cells[0]);
    const emitter = /`([A-Za-z_]\w*)`/.exec(cells[1]);
    if (!status || !emitter) throw new Error(`§7.1 row has no status or no emitter symbol: ${cells[0]}`);
    rows.push({ source: cells[0], emitter: emitter[1], text: cells[2], status: Number(status[1]) });
  }
  return rows;
}

const serverRows = parseServerComposedTable();
const literals = goStringLiterals();
const funcNames = goFuncNames();

/** The Go literal this row is the doc's rendering of, or undefined. */
const literalFor = (row: ServerRow) =>
  literals.find((lit) => goLiteralIsASentence(lit) && goLiteralPattern(lit).test(row.text));

describe("user-drives-prompt §7.1 — the server-composed table matches the Go source", () => {
  it("finds all 14 server-composed rows", () => {
    expect(serverRows.length).toBe(14);
  });

  it.each(serverRows.map((r) => [r.source, r] as const))(
    "%s is a string the Go side actually emits",
    (_source, row) => {
      // toBeDefined() would print "undefined"; the row's own text is what the
      // reader needs in order to go look for it.
      if (!literalFor(row)) {
        throw new Error(
          `no literal in ${GO_DIRS.join(" / ")} matches this §7.1 row — the code is the truth for a ` +
            `server-composed string, so either correct the row or delete it:\n  ${row.text}`,
        );
      }
    },
  );

  it.each(serverRows.map((r) => [r.source, r] as const))("%s names a real emitter", (_source, row) => {
    expect([...funcNames].includes(row.emitter)).toBe(true);
  });

  // The status half of the drift, checked against the statuses the Go API tests
  // already pin: each `want:`/`msg:` case whose message belongs to a row must
  // agree with that row's parenthesised code. Not every row has such a case —
  // the count below is the floor that stops this degrading to nothing.
  it("agrees with the statuses internal/api/user_drives_test.go pins", () => {
    const src = readFileSync(resolve(process.cwd(), "../internal/api/user_drives_test.go"), "utf8");
    const codes: Record<string, number> = {
      BadRequest: 400,
      Forbidden: 403,
      NotFound: 404,
      Conflict: 409,
      UnprocessableEntity: 422,
    };
    let checked = 0;
    for (const m of src.matchAll(/want:\s*http\.Status(\w+),\s*msg:\s*(?:"([^"]*)"|`([^`]*)`)/g)) {
      const want = codes[m[1]];
      const msg = m[2] ?? m[3];
      for (const row of serverRows) {
        const lit = literalFor(row);
        if (!lit) continue;
        // The pinned message is a SUBSTRING of the refusal, so it either sits
        // inside one fixed run of the literal or starts with one (the case
        // where the test spells a concrete value the literal interpolates).
        if (!goLiteralChunks(lit).some((c) => c.includes(msg) || msg.startsWith(c))) continue;
        checked++;
        expect(`${row.source} -> ${row.status}`).toBe(`${row.source} -> ${want}`);
      }
    }
    expect(checked).toBeGreaterThanOrEqual(6);
  });
});

// ---------------------------------------------------------------------------
// §7.7 — DRIVE_MEMBER's refusals, across the language boundary.
//
// The suite above proves TS == doc for all 140 keys. §7.7 is titled
// "DRIVE_MEMBER — refusals (server-composed)", and that title is the hole this
// block closes: no production TypeScript renders those eight constants. The
// bytes a member is actually refused with are composed in Go (user_drives_run.go,
// user_drives_resolve.go, runner/mount.go), so before this block the canon pin
// compared TS to the doc while the SHIPPED string came from a third copy nothing
// compared to either — and the Go tests that cover them HAND-RETYPE the
// sentence, which is exactly the Go-to-Go anti-pattern this file's header says
// it eliminated. A counterfactual proved it: rewording REFUSED_WRITABLE in the
// server AND its Go test copies left `go test ./internal/api/` and the whole UI
// canon suite green, with the frozen canon left describing bytes nobody emits.
//
// The check is a SHAPE comparison, not a substring hunt: a Go format verb and a
// doc {placeholder} both become one hole, %q brings the quotes it renders, and
// the two envelopes the server adds around the frozen sentence are stripped from
// the doc side rather than pretended away —
//   - `drive: ` — driveRefusal() (internal/api/user_drives.go), on all six
//     REFUSED_* that go through refuseDrive / errDriveUnmountable;
//   - `workspace_mounts[0]: ` — the caller's own field prefix, which the canon
//     spells because §7.7 says it does (`[0]` is the mount's position).
// Both are re-derived from the Go source below, so neither can rot into a fudge.
const HOLE = "\u0000";
const GO_VERB = /%[#+\- 0]*\d*(?:\.\d+)?[a-zA-Z]/g;

/** A Go literal as the shape its runtime output has: verbs become holes, %q keeps the quotes it renders. */
const goShape = (lit: string) => lit.replace(GO_VERB, (v) => (v.endsWith("q") ? `"${HOLE}"` : HOLE));
/** A doc cell as the same shape: `{name}` is the hole the verb fills. */
const docShape = (text: string) => text.replace(/\{[A-Za-z_]+\}/g, HOLE);

/** key -> frozen string, for the rows of ONE `### 7.N` table. */
function parseOneSection(heading: RegExp): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      inSection = heading.test(line);
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key" || /^:?-+:?$/.test(cells[0])) continue;
    rows.set(unmono(cells[0]), unmono(cells[cells.length - 1]));
  }
  return rows;
}

const sec77 = parseOneSection(/^### 7\.7\b/);
const mountGo = readFileSync(resolve(process.cwd(), "../internal/runner/mount.go"), "utf8");
const userDrivesGo = readFileSync(resolve(process.cwd(), "../internal/api/user_drives.go"), "utf8");
const DRIVE_TARGET = (/const DriveTarget = "([^"]+)"/.exec(mountGo) ?? [])[1];
const DRIVE_PREFIX = "drive: ";
const MOUNT_PREFIX = "workspace_mounts[0]: ";

// The one row whose Go literal legitimately carries MORE than the frozen
// sentence: types.DriveHomeStricterRuleClause is APPENDED (the §7.1
// server-composed class — the canon is frozen, so the k8s-only clause could not
// be worded into it) and strings.TrimSpace removes the join when it is empty.
const ALLOWED_TAIL: Record<string, string> = {
  "REFUSED_HOME_INVALID(claim)": ` ${HOLE}`,
};

// REFUSED_BACKEND's `{reason}` is not a format verb — it is driveMountFor's own
// prose, a different clause per cause (§7.7's own note). So its row is checked
// as the ENVELOPE it is: every backend refusal the server composes must open
// with the frozen prefix and close the parenthesis, and there must be several.
const COMPOSED = "REFUSED_BACKEND(reason)";

/** The doc row spelled the way the Go literal for it would be. */
function docCandidates(text: string): string[] {
  const base = docShape(text);
  const out = [base];
  for (const p of [DRIVE_PREFIX, MOUNT_PREFIX]) {
    if (base.startsWith(p)) out.push(base.slice(p.length));
  }
  // The reserved target is a CONSTANT the canon spells out where the server
  // interpolates runner.DriveTarget (§7.7: "the frozen string spells the first
  // mount's, so the canon equals the server's bytes").
  if (DRIVE_TARGET) {
    for (const c of [...out]) {
      if (c.includes(DRIVE_TARGET)) out.push(c.split(DRIVE_TARGET).join(HOLE));
    }
  }
  return out;
}

describe("user-drives-prompt §7.7 — the member refusals match the Go source", () => {
  const shapes = literals.map(goShape);

  it("finds all 8 refusal rows", () => {
    expect([...sec77.keys()].sort()).toEqual(
      [
        "DENIED_DRIVE(name)",
        "REFUSED_BACKEND(reason)",
        "REFUSED_HOME_INVALID(claim)",
        "REFUSED_HOME_MISSING(name)",
        "REFUSED_NO_GRANT",
        "REFUSED_PAUSED",
        "REFUSED_TARGET_RESERVED",
        "REFUSED_WRITABLE",
      ].sort(),
    );
  });

  it("strips only the envelopes the Go source actually adds", () => {
    // driveRefusal() — internal/api/user_drives.go
    expect(userDrivesGo).toContain(`return "${DRIVE_PREFIX}" + strings.TrimSpace(reason)`);
    // …and the mount prefix is the caller's own field convention, which
    // ValidateAuthoredTarget's refusal is documented to be given.
    expect(mountGo).toContain("is reserved for the user drive");
    expect(MOUNT_PREFIX.startsWith("workspace_mounts[")).toBe(true);
  });

  it("REFUSED_TARGET_RESERVED spells runner.DriveTarget itself", () => {
    expect(DRIVE_TARGET).toBe("/home/agent/drive");
    expect(sec77.get("REFUSED_TARGET_RESERVED")).toContain(DRIVE_TARGET as string);
  });

  it.each([...sec77.keys()].filter((k) => k !== COMPOSED).map((k) => [k] as const))(
    "%s is a string the Go side actually composes",
    (key) => {
      const text = sec77.get(key) as string;
      const cands = docCandidates(text);
      const tail = ALLOWED_TAIL[key] ?? "";
      const hit = shapes.find(
        (g) => cands.includes(g) || (tail !== "" && g.endsWith(tail) && cands.includes(g.slice(0, -tail.length))),
      );
      if (!hit) {
        throw new Error(
          `no literal in ${GO_DIRS.join(" / ")} composes this §7.7 row — §7.7 is SERVER-composed, so ` +
            `the Go source is the truth for its bytes; reword both sides or neither:\n  ${text}`,
        );
      }
    },
  );

  it(`${COMPOSED}: every backend refusal the server composes fits the frozen envelope`, () => {
    const [open, close] = docShape(sec77.get(COMPOSED) as string).split(HOLE);
    expect(close).toBe(")");
    const bare = open.startsWith(DRIVE_PREFIX) ? open.slice(DRIVE_PREFIX.length) : open;
    const fits = shapes.filter((g) => g.includes(bare));
    // The arms driveMountFor / driveShareBindFailure / resolveUserDrive
    // compose: an unsupported runner, a backend that dispatches elsewhere, an
    // unreachable share, and the two template-vs-backend refusals.
    expect(
      fits.length,
      `no Go literal opens with REFUSED_BACKEND's frozen prefix: ${bare}`,
    ).toBeGreaterThanOrEqual(4);
    for (const g of fits) {
      expect(g.trimEnd().endsWith(close), `a backend refusal escapes the frozen envelope: ${g}`).toBe(true);
    }
  });
});
