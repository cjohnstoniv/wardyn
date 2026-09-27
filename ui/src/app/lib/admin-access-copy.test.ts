/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { ADMIN_ACCESS_BANNER, SIGNIN_HELP } from "./access-posture-copy";
import { SIGNIN_HELP_LINK_LABEL } from "./people-access-copy";
import { parseFrozenTables } from "./copy-doc-parity";

// The mock round's whole value is that it stays CHECKABLE (the sign-in/
// ado-entra precedent, copy-doc-parity.ts's parseFrozenTables(), T-66): this
// suite PARSES docs/design/admin-access-canon.md's "## Frozen strings" table
// back out of the doc and compares every UI-owned key.
//
// Owner ruling 2026-09-25 (#726): the doc's table was reordered to
// `Id | Where | String` (purely structural — parseFrozenTables() always
// takes the LAST cell as the frozen value, and the doc used to put `Where`
// last) so it parses cleanly here.
//
// The table's 29 rows are not all this module's to carry: `sso_rbac.*` and
// `sign_in_help_url.*` are server-composed setup-check text
// (internal/api/setup_checks.go / setup_checks_siteconfig.go), the two
// `refusal: *` rows are Go 400 messages (internal/api/site_config_signin_
// help.go), and the two `SSH keys chip*` rows are an inline JSX literal in
// ssh-keys.tsx with no exported constant. Only the ADMIN_ACCESS_BANNER.*,
// SIGNIN_HELP.* and SIGNIN_HELP_LINK_LABEL rows (17 of 29, counting #491's
// BODY_DEFAULT_ROLE) live in access-posture-copy.ts / people-access-copy.ts,
// per the build-queue spec and the owner's own scoping — those are the 17
// pinned below.
const DOC = resolve(process.cwd(), "../docs/design/admin-access-canon.md");

// The table's header cell reads "Id", not "Key" — the only literal
// parseFrozenTables() skips as a header — so the raw parse carries one
// spurious "Id" -> "String" row (the literal header text), filtered out here
// rather than in the shared parser.
const rawDoc = parseFrozenTables(DOC, /^## Frozen strings/);
const doc = new Map([...rawDoc].filter(([key, value]) => !(key === "Id" && value === "String")));

// SIGNIN_HELP.COUNTER is a function ((n: number) => `${n} / 1000`); the doc
// spells its placeholder inline as "{n}" rather than the `KEY(args)` shape
// splitKey()/renderFromNamespaces() parse, so it's called directly here with
// its own placeholder text (the same convention those helpers use elsewhere).
const rendered: Record<string, string> = {
  "ADMIN_ACCESS_BANNER.TITLE": ADMIN_ACCESS_BANNER.TITLE,
  "ADMIN_ACCESS_BANNER.BODY": ADMIN_ACCESS_BANNER.BODY,
  "ADMIN_ACCESS_BANNER.ACTION": ADMIN_ACCESS_BANNER.ACTION,
  "ADMIN_ACCESS_BANNER.BODY_DEFAULT_ROLE": ADMIN_ACCESS_BANNER.BODY_DEFAULT_ROLE,
  "SIGNIN_HELP.TITLE": SIGNIN_HELP.TITLE,
  "SIGNIN_HELP.LEAD": SIGNIN_HELP.LEAD,
  "SIGNIN_HELP.TEXT_LABEL": SIGNIN_HELP.TEXT_LABEL,
  "SIGNIN_HELP.TEXT_PLACEHOLDER": SIGNIN_HELP.TEXT_PLACEHOLDER,
  "SIGNIN_HELP.TEXT_HINT": SIGNIN_HELP.TEXT_HINT,
  "SIGNIN_HELP.COUNTER": (SIGNIN_HELP.COUNTER as unknown as (n: string) => string)("{n}"),
  "SIGNIN_HELP.URL_LABEL": SIGNIN_HELP.URL_LABEL,
  "SIGNIN_HELP.URL_PLACEHOLDER": SIGNIN_HELP.URL_PLACEHOLDER,
  "SIGNIN_HELP.URL_HINT": SIGNIN_HELP.URL_HINT,
  "SIGNIN_HELP.EMPTY_NOTE": SIGNIN_HELP.EMPTY_NOTE,
  "SIGNIN_HELP.PREVIEW_HEADING": SIGNIN_HELP.PREVIEW_HEADING,
  "SIGNIN_HELP.APPLIES_NOTE": SIGNIN_HELP.APPLIES_NOTE,
  SIGNIN_HELP_LINK_LABEL: SIGNIN_HELP_LINK_LABEL,
};

describe("admin-access-copy — admin-access-canon.md, #489", () => {
  it("finds all 29 rows in the doc's Frozen strings table", () => {
    expect(doc.size).toBe(29);
  });

  it("the 17 UI-owned rows are byte-exact", () => {
    for (const key of Object.keys(rendered)) {
      expect(rendered[key], key).toBe(doc.get(key));
    }
  });

  it("every UI-owned row is accounted for, and none pinned twice", () => {
    expect(Object.keys(rendered).sort()).toEqual(
      [
        "ADMIN_ACCESS_BANNER.TITLE",
        "ADMIN_ACCESS_BANNER.BODY",
        "ADMIN_ACCESS_BANNER.ACTION",
        "ADMIN_ACCESS_BANNER.BODY_DEFAULT_ROLE",
        "SIGNIN_HELP.TITLE",
        "SIGNIN_HELP.LEAD",
        "SIGNIN_HELP.TEXT_LABEL",
        "SIGNIN_HELP.TEXT_PLACEHOLDER",
        "SIGNIN_HELP.TEXT_HINT",
        "SIGNIN_HELP.COUNTER",
        "SIGNIN_HELP.URL_LABEL",
        "SIGNIN_HELP.URL_PLACEHOLDER",
        "SIGNIN_HELP.URL_HINT",
        "SIGNIN_HELP.EMPTY_NOTE",
        "SIGNIN_HELP.PREVIEW_HEADING",
        "SIGNIN_HELP.APPLIES_NOTE",
        "SIGNIN_HELP_LINK_LABEL",
      ].sort(),
    );
  });
});
