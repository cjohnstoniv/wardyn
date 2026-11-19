/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, MEMBER_PRINCIPAL, asRealSecurityAdmin, asRealUnitAdmin, consoleAPI, gotoConsole, grantUnitAdmin, navTo, sidebarLink } from "./fixtures";
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { CHANGES } from "../src/app/lib/governance-copy";
import { CHANGE_NOTE } from "../src/app/components/wardyn/copy/change-note";
import { aheadByHours } from "../src/app/lib/test-clock";

// Requests and the newer kinds on Governance › Changes. The list read is spliced so each row can be of a kind
// the plain harness has no way to hold: the console reads it as the server would send it. A security admin
// is the viewer; `proposed_by` decides whether a row is theirs.
const VIEWER = "e2e-security-admin";

function elevation(proposedBy: string) {
  return {
    id: "00000000-0000-4000-8000-0000000000e1",
    target_kind: "governance_elevation",
    op: "create",
    target_key: "00000000-0000-4000-8000-0000000000e1",
    state: "pending",
    proposed_by: proposedBy,
    proposed_at: aheadByHours(-1),
    expires_at: aheadByHours(23),
    diff: { after: { unit_id: "00000000-0000-4000-8000-0000000000a1", unit_name: "Payments", subject: "alice", duration: "4h" }, changed: ["duration"] },
    note: { reason: "Production incident", ticket: "SUP-1" },
  };
}

async function openChanges(page: Page, list: object[]) {
  // The list route only: `*` stops at "/", so a change read by id is not spliced.
  await page.route("**/api/v1/governance/changes*", (route) => (route.request().method() === "GET" ? route.fulfill({ json: list }) : route.fallback()));
  await asRealSecurityAdmin(page);
  await gotoConsole(page, "admin");
  await navTo(page, "Governance");
  await page.getByRole("tab", { name: CHANGES.TAB_COUNT(list.length) }).click();
}

test.describe("governance — requests on the Changes tab", () => {
  test("an elevation change from another person reads as a request", async ({ page }) => {
    await openChanges(page, [elevation("alice")]);
    const label = `${CHANGES.KIND.governance_elevation} · ${CHANGES.OP.create}`;
    const row = page.getByRole("row").filter({ has: page.getByRole("button", { name: label, exact: true }) });
    await expect(row).toContainText("4 hours from approval · SUP-1");
    await expect(row).toContainText("alice → Payments");

    await page.getByRole("button", { name: label, exact: true }).click();
    const drawer = page.getByRole("dialog");
    await expect(drawer.getByText(CHANGE_NOTE.ELEV_LENGTH_VALUE("4 hours"), { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: CHANGES.REJECT, exact: true })).toBeEnabled();
    await expect(drawer.getByRole("button", { name: CHANGES.APPROVE, exact: true })).toBeEnabled();
  });

  test("the same change from the viewer offers Withdraw request", async ({ page }) => {
    await openChanges(page, [elevation(VIEWER)]);
    await page.getByRole("button", { name: `${CHANGES.KIND.governance_elevation} · ${CHANGES.OP.create}`, exact: true }).click();
    const drawer = page.getByRole("dialog");
    await expect(drawer.getByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST, exact: true })).toBeEnabled();
    await expect(drawer.getByText(CHANGE_NOTE.OWN_REQUEST, { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: CHANGES.APPROVE, exact: true })).toBeDisabled();
  });

  test("a deciders change reads Deciders · Set", async ({ page }) => {
    await openChanges(page, [{ ...elevation("alice"), target_kind: "governance_unit_deciders", op: "upsert", diff: { changed: [] }, note: undefined }]);
    await expect(page.getByRole("button", { name: `${CHANGES.KIND.governance_unit_deciders} · ${CHANGES.OP.upsert}`, exact: true })).toBeVisible();
  });
});

// A unit admin's own change. Nothing is spliced: U has one unit admin, so their membership write is held, and with
// nobody else to approve it only security can decide it. The proposer still sees it, and may note or withdraw it.
// A unit admin writes memberships only for a person who already holds one in their units, so the member is put
// in U first with the admin token.
test.describe("a unit admin's own change", () => {
  test("they read that only security can decide it, and add a note", async ({ page }) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const made = await page.request.post("/api/v1/governance/units", {
      headers: auth,
      data: {
        name: `own-change-${randomUUID().slice(0, 8)}`,
        ceiling: {
          allowed_domains: ["api.anthropic.com"],
          denied_domains: [],
          first_use_approval: "deny_with_review",
          min_confinement_class: "CC3",
          auto_stop_after_sec: 3600,
          eligible_grants: [],
          tool_rules: [{ tool: "*", effect: "allow" }],
        },
        limits: {},
      },
    });
    expect(made.status(), await made.text()).toBe(201);
    const unitId = (await made.json()).unit.id as string;
    let changeId = "";
    let memberId = "";
    try {
      const seeded = await page.request.post("/api/v1/governance/memberships", {
        headers: auth,
        data: { subject_type: "user", subject: MEMBER_PRINCIPAL, unit_id: unitId, priority: 0 },
      });
      expect(seeded.status(), await seeded.text()).toBe(201);
      memberId = (await seeded.json()).id as string;
      await grantUnitAdmin(page, unitId);
      await asRealUnitAdmin(page);
      // A unit admin's membership write is always held for a second person.
      const held = await consoleAPI(page, "POST", "/api/v1/governance/memberships", {
        subject_type: "user",
        subject: MEMBER_PRINCIPAL,
        unit_id: unitId,
        priority: 1,
      });
      expect(held.status, held.text).toBe(202);
      changeId = JSON.parse(held.text).pending_change.id;

      await gotoConsole(page);
      await sidebarLink(page, "Governance").click();
      await page.getByRole("tab", { name: new RegExp(`^${CHANGES.TAB}`) }).click();
      await page.getByRole("button", { name: new RegExp(`^${CHANGES.KIND.governance_membership} ·`) }).first().click();

      const drawer = page.getByRole("dialog");
      await expect(drawer.getByText(CHANGE_NOTE.OWN_SECURITY_TITLE, { exact: true })).toBeVisible();
      await expect(drawer.getByText(CHANGE_NOTE.OWN_SECURITY_BODY, { exact: true })).toBeVisible();
      await expect(drawer.getByText(CHANGES.OWN_NOTE, { exact: true })).toHaveCount(0);
      await expect(drawer.getByRole("button", { name: CHANGES.APPROVE, exact: true })).toBeDisabled();

      await drawer.getByRole("button", { name: CHANGE_NOTE.ADD, exact: true }).click();
      await drawer.getByLabel(CHANGE_NOTE.WHY_LABEL, { exact: true }).fill("Give the member this profile");
      await drawer.getByRole("button", { name: CHANGE_NOTE.SAVE, exact: true }).click();
      await expect(page.getByText(CHANGE_NOTE.SAVED, { exact: true })).toBeVisible();
    } finally {
      if (changeId) await page.request.post(`/api/v1/governance/changes/${changeId}/reject`, { headers: auth, data: {} });
      if (memberId) await page.request.delete(`/api/v1/governance/memberships/${memberId}`, { headers: auth });
      await page.request.delete(`/api/v1/governance/units/${unitId}`, { headers: auth });
    }
  });
});
