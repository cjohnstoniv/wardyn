/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import { CAPABILITY_KINDS, KIND, PERM } from "../src/app/lib/permissions-copy";

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
    await gotoConsole(page);
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
    await gotoConsole(page);
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
    await gotoConsole(page);
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
    await gotoConsole(page);
    await navTo(page, "Permissions");

    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_OFF_BODY)).toBeVisible();
    await dialog.getByRole("button", { name: PERM.ENFORCE_STOP, exact: true }).click();

    await expect(page.getByText(KIND.egress_host.unenforced)).toBeVisible();
    await expect(page.getByText(PERM.CHIP_ON, { exact: true })).toHaveCount(0);
  });

  test("removing the grant names who loses it, then empties the table", async ({ page }) => {
    await gotoConsole(page);
    await navTo(page, "Permissions");

    await page.getByRole("button", { name: `${PERM.REMOVE} ${KIND.egress_host.label} ${HOST}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.REMOVE_CONFIRM(WHO))).toBeVisible();
    await dialog.getByRole("button", { name: PERM.REMOVE, exact: true }).click();

    await expect(page.getByText(PERM.EMPTY_TITLE)).toBeVisible();
    await expect(page.getByText(PERM.EMPTY_BODY)).toBeVisible();
  });

  test("enforcing with nothing granted is the lockout guard, and it is unmissable", async ({ page }) => {
    await gotoConsole(page);
    await navTo(page, "Permissions");

    await page.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.workspace.label}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(PERM.ENFORCE_ON_ZERO)).toBeVisible();
    // Walk away from it: nothing was written.
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByText(PERM.CHIP_ON, { exact: true })).toHaveCount(0);
  });
});
