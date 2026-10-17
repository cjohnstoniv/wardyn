/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, asRealSecurityAdmin, asRealSecurityAdmin2, consoleAPI } from "./fixtures";
import { CHANGES, GOVERNANCE as GOV } from "../src/app/lib/governance-copy";

// ---------------------------------------------------------------------------
// Four-eyes on governance writes (0.8.6): a covered write is held for a second human, the console shows
// it on the Changes tab, and a different human approves it there.
//
// The daemon must boot with WARDYN_GOVERNANCE_SECOND_HUMAN on, which turns EVERY covered governance write
// of every other spec into a held change. So the switch is on for this spec alone: ci.yml runs it in its
// own step with the switch set, and every other run skips it here, by name, via the allowlist.
//
// Two seeded humans (scripts/e2e-backend.sh): e2e-security-admin proposes, e2e-security-admin-2 approves,
// each with its own principal AND its own mailbox, since one mailbox counts as one human. The admin
// token is only the setup writer (its writes are the break-glass and apply directly); it never approves.
// ---------------------------------------------------------------------------
test.skip(
  process.env.WARDYN_GOVERNANCE_SECOND_HUMAN !== "true",
  "needs WARDYN_GOVERNANCE_SECOND_HUMAN=true in the daemon: the switch changes every covered governance write, so only this spec runs with it on",
);

// One profile, one change, three people: the steps read what the one before wrote.
test.describe.configure({ mode: "serial" });

const NAME = `four-eyes-${randomUUID().slice(0, 8)}`;
const CEILING = {
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
};
const CHANGE_LABEL = `${CHANGES.KIND.governance_profile} · ${CHANGES.OP.update}`;

test.describe("governance — a covered write waits for a second human", () => {
  test("the first security admin proposes a widening edit, and the profile is unchanged", async ({ page }) => {
    // Setup as the admin token: the break-glass, applied directly.
    const created = await page.request.post("/api/v1/governance/profiles", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      data: { name: NAME, ceiling: CEILING, limits: { max_concurrent_runs: 3 } },
    });
    expect(created.status()).toBe(201);

    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("button", { name: `${GOV.EDIT} ${NAME}`, exact: true }).click();
    // Raising the cap widens the profile, so it is held rather than applied.
    await page.locator("#governance-limit-concurrent").fill("10");
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();

    // Submitted, never saved: the note is up, the editor is closed, and the row still reads the old cap.
    await expect(page.getByText(CHANGES.SUBMITTED_TITLE, { exact: true })).toBeVisible();
    await expect(page.getByText(CHANGES.SUBMITTED_BODY, { exact: true })).toBeVisible();
    await expect(page.getByTestId("governance-profile-editor")).toHaveCount(0);
    const row = page.getByRole("row").filter({ hasText: NAME });
    await expect(row.getByText(GOV.LIMIT_QUOTA_LABEL(3))).toBeVisible();

    const snapshot = await consoleAPI(page, "GET", "/api/v1/governance");
    expect(snapshot.status).toBe(200);
    const stored = (JSON.parse(snapshot.text).profiles as { name: string; limits: { max_concurrent_runs?: number } }[]).find(
      (p) => p.name === NAME,
    );
    expect(stored?.limits.max_concurrent_runs).toBe(3);
  });

  test("the same security admin sees Approve disabled on their own proposal", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: CHANGES.TAB_COUNT(1) }).click();

    await page.getByRole("button", { name: CHANGE_LABEL, exact: true }).click();
    const drawer = page.getByRole("dialog");
    await expect(drawer.getByText(NAME, { exact: true }).first()).toBeVisible();
    // The server's own diff: the changed field, before and after.
    await expect(drawer.getByText("limits.max_concurrent_runs", { exact: true })).toBeVisible();
    await expect(drawer.getByText(CHANGES.OWN_NOTE, { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: CHANGES.APPROVE, exact: true })).toBeDisabled();
    await expect(drawer.getByRole("button", { name: CHANGES.REJECT, exact: true })).toBeEnabled();
  });

  test("the second security admin approves it in the drawer", async ({ page }) => {
    await asRealSecurityAdmin2(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: CHANGES.TAB_COUNT(1) }).click();

    await page.getByRole("button", { name: CHANGE_LABEL, exact: true }).click();
    const drawer = page.getByRole("dialog");
    await expect(drawer.getByText(CHANGES.OWN_NOTE, { exact: true })).toHaveCount(0);
    await drawer.getByRole("button", { name: CHANGES.APPROVE, exact: true }).click();

    await expect(page.getByText(CHANGES.TOAST_APPROVED, { exact: true })).toBeVisible();
    await expect(page.getByText(CHANGES.EMPTY_TITLE, { exact: true })).toBeVisible();
    await expect(page.getByRole("tab", { name: CHANGES.TAB, exact: true })).toBeVisible();
  });

  test("the profile shows the change", async ({ page }) => {
    await asRealSecurityAdmin2(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    const row = page.getByRole("row").filter({ hasText: NAME });
    await expect(row.getByText(GOV.LIMIT_QUOTA_LABEL(10))).toBeVisible();
    await expect(row.getByText(GOV.LIMIT_QUOTA_LABEL(3))).toHaveCount(0);
  });
});
