/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, it, expect } from "vitest";

import { ACCESS_ERROR, GUARD, PEOPLE, PREVIEW, SIGNIN } from "./people-access-copy";

// Sentinel byte-exact pins against docs/design/people-access-prompt.md §7 — a
// hand-retyped copy could silently drift (an em-dash swapped for a hyphen, a
// dropped word); these lock a sample verbatim.
describe("people-access-copy — sentinel byte-exact pins", () => {
  it("pins PEOPLE entries verbatim", () => {
    expect(PEOPLE.TABLE_LEAD).toBe(
      "A value — an Entra App Role, a groups-claim entry, or an email — mapped to admin or member.",
    );
    expect(PEOPLE.SHADOWED_BODY).toBe(
      "Your chart now maps this value too, and the chart always wins. This row is stored but has no effect until you remove the chart entry or delete this row.",
    );
    expect(PEOPLE.DEFAULT_ROLE_UNSET).toBe("Unset — anyone matching no mapping is denied at sign-in.");
  });

  it("pins ACCESS_ERROR.EMAIL_KEY_REFUSED byte-identical to access.go's accessEmailKeyRefused const", () => {
    expect(ACCESS_ERROR.EMAIL_KEY_REFUSED).toBe(
      "Email mappings are disabled on this install. Map an App Role or group instead, or opt in with WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS in your chart.",
    );
  });

  // Post-adjudication canon addition (backend review round, commit
  // 544467ed) — verbatim from the DOC's quoted string, which carries a
  // trailing period the Go accessStaleSnapshot const itself does NOT (the
  // component matches the server's raw message exactly, then renders THIS
  // fuller frozen copy — same pattern LOCKOUT_ERROR already used).
  it("pins ACCESS_ERROR.STALE_SNAPSHOT_ERROR verbatim, distinct from LOCKOUT_ERROR", () => {
    expect(ACCESS_ERROR.STALE_SNAPSHOT_ERROR).toBe(
      "your sign-in is too old to verify this change — sign in again before changing role mappings.",
    );
    expect(ACCESS_ERROR.STALE_SNAPSHOT_ERROR).not.toBe(ACCESS_ERROR.LOCKOUT_ERROR);
  });
});

// §7.3's four worked examples (the ones that actually fire — admin→admin and
// member→member are the suppressed no-op pairs and are never asked for a
// guard body). Each pin is the doc's own worked-example text, verbatim.
describe("people-access-copy — GUARD posture-flip worked examples (§7.3)", () => {
  it("allowlist empty, DEFAULT_ROLE unset", () => {
    expect(GUARD.FIRST_ROW_BODY("an admin", "be denied")).toBe(
      "Today, anyone matching no mapping signs in as an admin. After this mapping, they will be denied at their next sign-in.",
    );
    expect(GUARD.LAST_ROW_BODY("is denied at sign-in", "sign in as an admin")).toBe(
      "Today, anyone matching no mapping is denied at sign-in. After this removal, they will sign in as an admin at their next sign-in.",
    );
  });

  it("allowlist set, DEFAULT_ROLE=admin — the silent-widening case F-16 exists to catch", () => {
    expect(GUARD.FIRST_ROW_BODY("a member", "sign in as an admin")).toBe(
      "Today, anyone matching no mapping signs in as a member. After this mapping, they will sign in as an admin at their next sign-in.",
    );
    expect(GUARD.LAST_ROW_BODY("signs in as an admin", "sign in as a member")).toBe(
      "Today, anyone matching no mapping signs in as an admin. After this removal, they will sign in as a member at their next sign-in.",
    );
  });

  it("ALLOWLIST_NOTE and the ack label are pinned", () => {
    expect(GUARD.ALLOWLIST_NOTE).toBe("People on your operator allowlist stay admins either way.");
    expect(GUARD.GUARD_ACK_LABEL).toBe("I understand this changes who can sign in.");
  });
});

describe("people-access-copy — PREVIEW result trio + the DEFAULT/LEGACY split", () => {
  it("RESULT_MATCHED/RESULT_DEFAULT/RESULT_DENIED are pinned", () => {
    expect(PREVIEW.RESULT_MATCHED("admin", "Wardyn.Admin")).toBe("Would sign in as admin — matched by Wardyn.Admin.");
    expect(PREVIEW.RESULT_DEFAULT("member")).toBe("Would sign in as member — nothing matched, so your default role applies.");
    expect(PREVIEW.RESULT_DENIED).toBe("Would be denied at sign-in — nothing matched, and no default role is set.");
  });

  // RESULT_LEGACY is the DISTINCT empty-combined-map arm — must never read
  // like RESULT_DEFAULT (which implies a DEFAULT_ROLE decided it).
  it("RESULT_LEGACY explicitly says the map is unused, not that a default decided", () => {
    const legacy = PREVIEW.RESULT_LEGACY("admin");
    expect(legacy).toBe("Would sign in as admin — no mappings are configured; the operator allowlist decides.");
    expect(legacy).not.toContain("default role");
  });
});

describe("people-access-copy — SIGNIN arms (§7.7)", () => {
  it("NO_ROLE and EMAIL_VERIFIED_ABSENT both name 'your Wardyn admin', not a bare 'an operator'", () => {
    expect(SIGNIN.NO_ROLE).toMatch(/ask your wardyn admin/i);
    expect(SIGNIN.EMAIL_VERIFIED_ABSENT).toMatch(/ask your wardyn admin/i);
  });

  it("ROLE_CHECK_UNAVAILABLE is pinned", () => {
    expect(SIGNIN.ROLE_CHECK_UNAVAILABLE).toBe("Couldn't check your access — try again, or contact your admin.");
  });
});

// Casing rule (§7.2): {role} interpolated INSIDE a sentence is lowercase —
// "admin"/"security admin"/"member"; the ROLE_ADMIN/ROLE_SECURITY_ADMIN/
// ROLE_MEMBER chip labels are the only title-case forms, and a sentence takes
// the chip's lowercase via access-panel.tsx's roleLabelInSentence.
describe("people-access-copy — casing rule", () => {
  it("ROLE_ADMIN/ROLE_MEMBER are title case (chip labels only)", () => {
    expect(PEOPLE.ROLE_ADMIN).toBe("Admin");
    expect(PEOPLE.ROLE_MEMBER).toBe("Member");
  });

  it("every parameterized sentence template lowercases its {role}/{before}/{after} interpolation", () => {
    expect(GUARD.FIRST_ROW_BODY("an admin", "sign in as a member")).not.toMatch(/After this mapping, they will Sign/);
    expect(PREVIEW.RESULT_MATCHED("admin", "x")).toContain("as admin");
    expect(PREVIEW.RESULT_MATCHED("admin", "x")).not.toContain("as Admin");
  });

  // THE DOC HALF of the same rule (R4/F033). renderPreviewResult now lowers a
  // THIRD tier into RESULT_MATCHED/_DEFAULT/_LEGACY via roleLabelInSentence
  // (access-panel.tsx), so a security_admin verdict reads "Would sign in as
  // security admin — matched by …" where it used to read "member". §7.2's rule
  // enumerated only admin/member, which made the doc false about a shipped
  // string; these pin the enumeration and the one derivation that owns it, in
  // both docs, so neither can drift back.
  it("people-access-prompt §7.2's casing rule names the lowercase third tier and its derivation", () => {
    const doc = readFileSync(resolve(process.cwd(), "../docs/design/people-access-prompt.md"), "utf8");
    const rule = doc.slice(doc.indexOf("**Casing rule:**"), doc.indexOf("`COL_ADDED`'s chart-row"));
    expect(rule).not.toBe("");
    expect(rule).toContain("`security admin`");
    expect(rule).toContain("ROLE_SECURITY_ADMIN");
    expect(rule).toContain("roleLabelInSentence");
    // The in-sentence form is exactly the chip's lowercase — the module is the
    // truth for the chip, so the doc's third-tier word is derived, not retyped.
    expect(PEOPLE.ROLE_SECURITY_ADMIN).toBe("Security admin");
    expect(rule).toContain(PEOPLE.ROLE_SECURITY_ADMIN.toLowerCase());
    // THE MODULE HALF (R4/CANON-F033-B). The doc is not the only place this
    // rule is written down: people-access-copy.ts's own header transcribes it,
    // and that transcription enumerated admin/member only. Rule (b) moves the
    // module WITH the doc, so pin the module's copy of the rule too — a doc
    // fixed alone rots back the moment the next author reads the source.
    const mod = readFileSync(resolve(process.cwd(), "src/app/lib/people-access-copy.ts"), "utf8");
    const modRule = mod.slice(mod.indexOf("// Casing rule (§7.2)"), mod.indexOf("export const PEOPLE"));
    expect(modRule).not.toBe("");
    expect(modRule).toContain(PEOPLE.ROLE_SECURITY_ADMIN.toLowerCase());
    expect(modRule).toContain("ROLE_SECURITY_ADMIN");
    expect(modRule).toContain("roleLabelInSentence");
    expect(modRule).not.toMatch(/never interpolated into a sentence/);
  });

  it("governance-prompt §7.9 agrees — title case as the CHIP, lowercased in a sentence", () => {
    const gov = readFileSync(resolve(process.cwd(), "../docs/design/governance-prompt.md"), "utf8");
    const from = gov.indexOf("`ROLE_SECURITY_ADMIN` is the picker option");
    expect(from).toBeGreaterThan(-1);
    const note = gov.slice(from, gov.indexOf("\n\n", from));
    expect(note).toContain("`security admin`");
    expect(note).toContain("roleLabelInSentence");
    // The statement F033 falsified, gone: it IS interpolated, lowercased.
    expect(note).not.toMatch(/title case and never interpolated into a sentence/);
    // THE MODULE HALF (R4/CANON-F033-B): governance-copy.ts cites §7.9 as its
    // authority and quoted that same retired sentence verbatim, so the doc fix
    // left the code contradicting the doc it points at.
    const govMod = readFileSync(resolve(process.cwd(), "src/app/lib/governance-copy.ts"), "utf8");
    const govNote = govMod.slice(govMod.indexOf("// RE-EXPORTED, never retyped"), govMod.indexOf("ROLE_SECURITY_ADMIN: PEOPLE.ROLE_SECURITY_ADMIN"));
    expect(govNote).not.toBe("");
    expect(govNote).not.toMatch(/never interpolated into a sentence/);
    expect(govNote).toContain("roleLabelInSentence");
  });
});
