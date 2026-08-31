/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

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

// Casing rule (§7.2): {role} interpolated INSIDE a sentence is lowercase; the
// ROLE_ADMIN/ROLE_MEMBER chip labels are the only title-case forms.
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
});
