/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, mockMemberRole, mockSecurityAdminRole, navTo, navToRoute } from "./fixtures";
import { CAPABILITY_KINDS, KIND, PERM, PERM_DRAFT } from "../src/app/lib/permissions-copy";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";

// ---------------------------------------------------------------------------
// Permissions screen e2e (lane: permissions, port 8088, db wardyn_e2e).
//
// Real backend, real writes: the four operatorOnly routes behind this screen
// (GET /permissions, POST/DELETE /permissions/grants, PUT
// /permissions/enforcement) are all reachable with the seeded admin bearer, so
// this lane drives the whole loop — empty install, add a grant, enforce the
// kind, stop enforcing, remove the grant — against Postgres rather than a mock.
//
// Every expected string is IMPORTED from lib/permissions-copy.ts, never
// retyped: the canon is the contract, and a spec that hardcodes the wording
// would keep passing while the screen drifted away from the reviewed mock.
//
// Serial: one backend, one grant table; a mutating test must not run beside a
// read-only assertion about the same rows.
test.describe.configure({ mode: "serial" });

const HOST = "*.github.com";
const WHO = "alice@corp.example";

test.describe("permissions — the admin surface, end to end", () => {
  test("a fresh install: nothing enforced, no grants, and it says so", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await expect(page.getByRole("heading", { name: PERM.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByText(PERM.DEFAULT_POSTURE)).toBeVisible();
    // The two standing facts — the doctrine and the admin exemption.
    await expect(page.getByText(PERM.DOCTRINE)).toBeVisible();
    await expect(page.getByText(PERM.EXEMPT)).toBeVisible();
    await expect(page.getByText(PERM.EMPTY_TITLE)).toBeVisible();

    // All four kinds, each showing the OFF-state consequence — the row that
    // proves the default posture is 0.5 byte for byte.
    for (const k of CAPABILITY_KINDS) {
      await expect(page.getByText(KIND[k].unenforced)).toBeVisible();
    }
    // getByText(string) is a case-insensitive SUBSTRING match in Playwright,
    // so "Enforced" would also match "Not enforced" — both chips are pinned
    // exactly, which is the whole point of the pair.
    await expect(page.getByText(PERM.CHIP_OFF, { exact: true })).toHaveCount(CAPABILITY_KINDS.length);
    await expect(page.getByText(PERM.CHIP_ON, { exact: true })).toHaveCount(0);
  });

  test("adding a grant lands the row — amber, and advisory until the kind is enforced", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    // By ROLE + exact name: getByLabel is a substring match, and "Host" is
    // also inside the "Enforcement Egress hosts" switch's own label.
    await page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true }).fill(WHO);
    await page.getByRole("textbox", { name: KIND.egress_host.valueLabel, exact: true }).fill(HOST);
    await page.getByRole("button", { name: PERM.ADD_CTA }).click();

    // exact throughout: role names and text are SUBSTRING matches by default,
    // and the row's own Remove button carries the host in its label too.
    const table = page.getByRole("table");
    await expect(table.getByRole("cell", { name: HOST, exact: true })).toBeVisible();
    await expect(table.getByText(PERM.EFFECT_ALLOW, { exact: true })).toBeVisible();
    // Recorded, not live: the kind is still off.
    await expect(page.getByText(PERM.ADVISORY)).toBeVisible();
    await expect(page.getByText(PERM.EMPTY_TITLE)).toHaveCount(0);
  });

  test("enforcing a kind confirms first, then flips the chip and the consequence", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` }).click();

    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_ON_TITLE(KIND.egress_host.label))).toBeVisible();
    // The affected-member count is deployment-derived, so pin the sentence, not
    // the number.
    await expect(dialog.getByText(/bounded by the grants below from their next request/)).toBeVisible();
    await dialog.getByRole("button", { name: PERM.ENFORCE_CONFIRM, exact: true }).click();

    await expect(page.getByText(KIND.egress_host.enforced)).toBeVisible();
    await expect(page.getByText(PERM.CHIP_ON, { exact: true })).toHaveCount(1);
    // An enforced kind never also claims to be advisory.
    await expect(page.getByText(PERM.ADVISORY)).toHaveCount(0);

    // It survives a reload — this was a real write, not local state.
    await page.reload();
    await expect(page.getByText(KIND.egress_host.enforced)).toBeVisible();
  });

  test("stop enforcing says denies still apply, and puts the off-state back", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_OFF_BODY)).toBeVisible();
    await dialog.getByRole("button", { name: PERM.ENFORCE_STOP, exact: true }).click();

    await expect(page.getByText(KIND.egress_host.unenforced)).toBeVisible();
    await expect(page.getByText(PERM.CHIP_ON, { exact: true })).toHaveCount(0);
  });

  test("removing the grant names who loses it, then empties the table", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await page.getByRole("button", { name: `${PERM.REMOVE} ${KIND.egress_host.label} ${HOST}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.REMOVE_CONFIRM(WHO))).toBeVisible();
    await dialog.getByRole("button", { name: PERM.REMOVE, exact: true }).click();

    await expect(page.getByText(PERM.EMPTY_TITLE)).toBeVisible();
    await expect(page.getByText(PERM.EMPTY_BODY)).toBeVisible();
  });

  test("enforcing with nothing granted is the lockout guard, and it is unmissable", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.workspace.label}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_ON_ZERO)).toBeVisible();
    // Walk away from it: nothing was written.
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByText(PERM.CHIP_ON, { exact: true })).toHaveCount(0);
  });
});

// 0.7.2's seventh kind (workspace-providers round) — added to CAPABILITY_KINDS
// on the same terms as `agent`/`integration`, so the fresh-install and
// snapshot-failure tests above already walk it for free. This is the one
// dedicated round trip: grant → enforce → the kind's own value column and
// enforcement consequence, real writes against Postgres like the egress_host
// walk above.
test.describe("permissions — the seventh kind (workspace_provider) round-trips like the others", () => {
  test.describe.configure({ mode: "serial" });
  const PROVIDER_ID = "github";
  const GRANTEE = "bob@corp.example";

  test("granting a provider id lands the row under its own value column", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true }).fill(GRANTEE);
    // The Kind picker defaults to the first CAPABILITY_KINDS entry
    // (egress_host) — the value field's label follows whichever kind is
    // selected, so workspace_provider has to be picked before its own
    // "Provider" field appears.
    await page.getByRole("combobox", { name: PERM.FIELD_CAPABILITY }).click();
    await page.getByRole("option", { name: KIND.workspace_provider.label }).click();
    await page.getByRole("textbox", { name: KIND.workspace_provider.valueLabel, exact: true }).fill(PROVIDER_ID);
    await page.getByRole("button", { name: PERM.ADD_CTA }).click();

    const table = page.getByRole("table");
    await expect(table.getByRole("cell", { name: PROVIDER_ID, exact: true })).toBeVisible();
    await expect(page.getByText(PERM.ADVISORY)).toBeVisible();
  });

  test("enforcing workspace_provider flips its own chip and consequence, independent of the others", async ({
    page,
  }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.workspace_provider.label}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_ON_TITLE(KIND.workspace_provider.label))).toBeVisible();
    await dialog.getByRole("button", { name: PERM.ENFORCE_CONFIRM, exact: true }).click();

    await expect(page.getByText(KIND.workspace_provider.enforced)).toBeVisible();
    // A neighbor kind (egress_host) stays unenforced — enforcing one kind
    // never flips a sibling's switch.
    await expect(page.getByText(KIND.egress_host.unenforced)).toBeVisible();

    await page.reload();
    await expect(page.getByText(KIND.workspace_provider.enforced)).toBeVisible();
  });

  test("removing the grant returns workspace_provider to its unenforced consequence", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    // Stop enforcing first (the lockout guard refuses a grantless enforced
    // kind's removal path the same as any other), then remove the grant.
    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.workspace_provider.label}` }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: PERM.ENFORCE_STOP, exact: true }).click();
    await expect(page.getByText(KIND.workspace_provider.unenforced)).toBeVisible();

    await page
      .getByRole("button", { name: `${PERM.REMOVE} ${KIND.workspace_provider.label} ${PROVIDER_ID}` })
      .click();
    await page.getByRole("alertdialog").getByRole("button", { name: PERM.REMOVE, exact: true }).click();
    await expect(page.getByRole("table").getByRole("cell", { name: PROVIDER_ID, exact: true })).toHaveCount(0);
  });
});

// R4/F015 + R4/F133 — the two places this screen used to state, as fact, an
// answer it never received. Both need a REAL failed response, which only the
// browser can produce, so they live here rather than only in RTL.
//
// Not in the serial block above: these route-intercept their own requests and
// write nothing.
test.describe("permissions — a snapshot, and a count, that never arrived", () => {
  test("a failed GET /permissions paints no kind state at all — never six 'Not enforced' chips", async ({
    page,
  }) => {
    await page.route("**/api/v1/permissions", (route) => route.fulfill({ status: 503, body: "{}" }));
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    // The screen is honest about not knowing: a retry, not a posture.
    await expect(page.getByRole("button", { name: /retry/i }).first()).toBeVisible();
    await expect(page.getByText(PERM.CHIP_OFF, { exact: true })).toHaveCount(0);
    await expect(page.getByText(PERM.CHIP_ON, { exact: true })).toHaveCount(0);
    await expect(page.getByText(PERM.DEFAULT_POSTURE)).toHaveCount(0);
    for (const k of CAPABILITY_KINDS) {
      await expect(page.getByText(KIND[k].unenforced)).toHaveCount(0);
    }
  });

  test("a refused GET /runs leaves the enforce dialog without a count, never '0 members'", async ({
    page,
  }) => {
    // The count is derived from the runs list; refusing it must not read as
    // "this affects nobody", which is the opposite of the lockout risk.
    // Refuse the runs LIST the enforce dialog counts from — but not the
    // console's boot probe (GET /runs?limit=1, core.ts probeAuth), which a 403
    // would turn into "unreachable" and park the console at the sign-in gate
    // before the screen under test ever renders.
    await page.route(
      (url) => url.pathname.endsWith("/api/v1/runs") && url.searchParams.get("limit") !== "1",
      (route) => route.fulfill({ status: 403, body: "{}" }),
    );
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_ON_BODY_UNKNOWN)).toBeVisible();
    await expect(dialog.getByText(/\b0 members\b/)).toHaveCount(0);
    await dialog.getByRole("button", { name: "Cancel" }).click();
  });
});

// R-2 (blind review, LOW): F4-F5's inert chip had no Playwright pin. A grant
// markInertGrants flags Inert can't be produced through the write boundary
// (canonicalGrantValue refuses or rewrites it on the way in), so this
// route-intercepts GET /permissions — the same idiom the block above uses —
// rather than writing one for real.
test.describe("permissions — an inert grant renders neutral, never live (F4-F5)", () => {
  test("a grant flagged inert renders the neutral Inert chip, never Allow/Deny", async ({ page }) => {
    await page.route("**/api/v1/permissions", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          grants: [
            {
              id: "11111111-1111-1111-1111-111111111111",
              subject_type: "user",
              subject: "alice@corp.example",
              capability: "secret",
              value: "STRIPE_LIVE_KEY",
              effect: "allow",
              created_at: "2026-08-01T00:00:00Z",
              created_by: "admin",
              inert: true,
            },
          ],
          enforcement: {},
        }),
      }),
    );
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");

    const table = page.getByRole("table");
    await expect(table.getByText(PERM_DRAFT.INERT_CHIP)).toBeVisible();
    await expect(table.getByText(PERM.EFFECT_ALLOW, { exact: true })).toHaveCount(0);
    await expect(table.getByText(PERM.EFFECT_DENY, { exact: true })).toHaveCount(0);
  });
});

// X3-F5 — /admin/permissions is a securityOps route hidden from a member's
// nav, so a member reaches it only by typing the URL. M-1b: Permissions moved
// under /admin/*, so the screen's own 403/500 render this test used to pin is
// now unreachable — the admin-view gate refuses a member before anything is
// fetched (admin-member-modes-design.md §2.3's refusal-page row). The harness
// bearer is always an admin server-side (fixtures.ts); what's real here is
// the console's own view gate, not the server's 403.
test.describe("Permissions — a member by URL is told the tier, not an outage", () => {
  test("the admin-view refusal, before /api/v1/permissions is ever asked", async ({ page }) => {
    await mockMemberRole(page);
    let fetched = false;
    await page.route("**/api/v1/permissions", async (route) => {
      fetched = true;
      await route.fallback();
    });
    await gotoConsole(page);
    await navToRoute(page, "/admin/permissions");

    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
    await expect(page.getByText(VIEW_REFUSAL.BODY)).toBeVisible();
    await expect(page.getByRole("button", { name: /retry/i })).toHaveCount(0);
    expect(fetched).toBe(false);
  });
});

// X2-F13 — the four write routes behind this screen (grant/enforcement) are
// gated on useSecurityOperator, not useOperator: a security admin is meant to
// actually USE them, unlike everywhere else in the console (SECURITY_ONLY_REASON
// above is that tier's OWN reason, distinct from OPERATOR_ONLY_REASON —
// policies.spec.ts's X2-F12 addition pins the contrast: /policies gives a
// security admin the same parked OPERATOR_ONLY_REASON a member gets, while
// THIS screen hands them the real form). governance.spec.ts:401 only ever
// asserted the nav LINK is visible to this tier — nothing exercised the form
// itself. Real writes, cleaned up at the end so the file's empty-table
// invariant survives a re-run.
//
// Ceiling (review F8): the harness bearer is admin server-side (fixtures.ts),
// so this proves the RENDER plus a working write path, not that the server
// itself authorizes security_operator — that's authz_test.go's classSecurity
// rows for POST /permissions/grants, DELETE /permissions/grants/{id}, and PUT
// /permissions/enforcement.
test.describe("Permissions — a security admin actually uses the write surface, not just sees the link (X2-F13)", () => {
  test.describe.configure({ mode: "serial" });
  const SEC_WHO = "carol@corp.example";
  const SEC_HOST = "*.security-admin-e2e.example";

  test("adds a grant and enforces the kind as security_admin, then cleans up", async ({ page }) => {
    await mockSecurityAdminRole(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");
    await expect(page.getByRole("heading", { name: PERM.TITLE, level: 1 })).toBeVisible();

    await page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true }).fill(SEC_WHO);
    await page.getByRole("textbox", { name: KIND.egress_host.valueLabel, exact: true }).fill(SEC_HOST);
    const addBtn = page.getByRole("button", { name: PERM.ADD_CTA });
    await expect(addBtn).toBeEnabled();
    await addBtn.click();

    const table = page.getByRole("table");
    await expect(table.getByRole("cell", { name: SEC_HOST, exact: true })).toBeVisible();

    const kindSwitch = page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` });
    await expect(kindSwitch).toBeEnabled();
    await kindSwitch.click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_ON_TITLE(KIND.egress_host.label))).toBeVisible();
    await dialog.getByRole("button", { name: PERM.ENFORCE_CONFIRM, exact: true }).click();
    await expect(page.getByText(KIND.egress_host.enforced)).toBeVisible();

    // Clean up: stop enforcing (the lockout guard requires it before a
    // grantless removal), then remove the grant.
    await kindSwitch.click();
    await page.getByRole("alertdialog").getByRole("button", { name: PERM.ENFORCE_STOP, exact: true }).click();
    await page.getByRole("button", { name: `${PERM.REMOVE} ${KIND.egress_host.label} ${SEC_HOST}` }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: PERM.REMOVE, exact: true }).click();
    await expect(page.getByText(PERM.EMPTY_TITLE)).toBeVisible();
  });
});
