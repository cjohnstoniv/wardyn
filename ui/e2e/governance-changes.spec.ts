/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, asRealSecurityAdmin, asRealSecurityAdmin2, consoleAPI } from "./fixtures";
import { CHANGES, GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { CHANGE_NOTE } from "../src/app/components/wardyn/copy/change-note";

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
// What the server says a direct save of CEILING omits from the deployment default: the held change carries the same notes.
let notes: string[] = [];
const CHANGE_LABEL = `${CHANGES.KIND.governance_unit} · ${CHANGES.OP.update}`;
const NOTE_REASON = "Raise the cap for the nightly build";
const NOTE_TICKET = "SUP-1";

test.describe("governance — a covered write waits for a second human", () => {
  test("the first security admin proposes a widening edit, and the profile is unchanged", async ({ page }) => {
    // Setup as the admin token: the break-glass, applied directly.
    const created = await page.request.post("/api/v1/governance/units", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      data: { name: NAME, ceiling: CEILING, limits: { max_concurrent_runs: 3 } },
    });
    expect(created.status()).toBe(201);
    notes = ((await created.json()).warnings ?? []) as string[];
    expect(notes.length, "the deployment default must differ from CEILING so there is a note to carry").toBeGreaterThan(0);

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
    // R1-0098: the proposer sees the same notes a direct save shows, above the submitted note.
    await expect(page.getByText(GOV.OMISSION_TITLE, { exact: true })).toBeVisible();
    for (const n of notes) await expect(page.getByText(n, { exact: true })).toBeVisible();
    const row = page.getByRole("row").filter({ hasText: NAME });
    await expect(row.getByText(GOV.LIMIT_QUOTA_LABEL(3))).toBeVisible();

    // The held change's id is known, so the note offers Add a note: a reason and a ticket go on the change.
    await page.getByRole("button", { name: CHANGE_NOTE.ADD, exact: true }).click();
    await page.getByLabel(CHANGE_NOTE.WHY_LABEL, { exact: true }).fill(NOTE_REASON);
    await page.getByLabel(CHANGE_NOTE.TICKET_LABEL, { exact: true }).fill(NOTE_TICKET);
    await page.getByRole("button", { name: CHANGE_NOTE.SAVE, exact: true }).click();
    await expect(page.getByText(CHANGE_NOTE.SAVED, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: CHANGE_NOTE.EDIT, exact: true })).toBeVisible();

    const snapshot = await consoleAPI(page, "GET", "/api/v1/governance");
    expect(snapshot.status).toBe(200);
    const stored = (JSON.parse(snapshot.text).units as { name: string; limits: { max_concurrent_runs?: number } }[]).find(
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
    // R1-0098: the approver's drawer carries the notes, under the existing heading.
    await expect(drawer.getByText(GOV.OMISSION_TITLE, { exact: true })).toBeVisible();
    for (const n of notes) await expect(drawer.getByText(n, { exact: true })).toBeVisible();
    // The proposer's note, and the way to change it.
    const why = drawer.getByTestId("change-note");
    await expect(why.getByText(CHANGE_NOTE.WHY_TITLE, { exact: true })).toBeVisible();
    await expect(why.getByText(NOTE_REASON, { exact: true })).toBeVisible();
    await expect(why.getByText(NOTE_TICKET, { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: CHANGE_NOTE.EDIT, exact: true })).toBeVisible();
    // No unit admin governs this profile, so only security can decide it: the own-change note says so
    // in place of OWN_NOTE.
    await expect(drawer.getByText(CHANGE_NOTE.OWN_SECURITY_TITLE, { exact: true })).toBeVisible();
    await expect(drawer.getByText(CHANGES.OWN_NOTE, { exact: true })).toHaveCount(0);
    await expect(drawer.getByRole("button", { name: CHANGES.APPROVE, exact: true })).toBeDisabled();
    await expect(drawer.getByRole("button", { name: CHANGES.REJECT, exact: true })).toBeEnabled();
  });

  test("the second security admin approves it in the drawer", async ({ page }) => {
    await asRealSecurityAdmin2(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: CHANGES.TAB_COUNT(1) }).click();

    // A profile change's approvers are security alone, so it stays listed under the filter.
    await page.getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY, exact: true }).click();
    await expect(page.getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY, exact: true })).toBeChecked();
    await expect(page.getByRole("button", { name: CHANGE_LABEL, exact: true })).toBeVisible();
    // Back to every change, so the empty state after the approval below is the plain one.
    await page.getByRole("radio", { name: CHANGE_NOTE.FILTER_ALL, exact: true }).click();

    await page.getByRole("button", { name: CHANGE_LABEL, exact: true }).click();
    const drawer = page.getByRole("dialog");
    const why = drawer.getByTestId("change-note");
    await expect(why.getByText(NOTE_REASON, { exact: true })).toBeVisible();
    await expect(why.getByText(NOTE_TICKET, { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: CHANGE_NOTE.EDIT, exact: true })).toHaveCount(0);
    await expect(drawer.getByRole("button", { name: CHANGE_NOTE.ADD, exact: true })).toHaveCount(0);
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
