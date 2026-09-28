/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, navTo } from "./fixtures";
import { ADO } from "../src/app/lib/ado-entra-copy";
import type { RunPolicySpec } from "../src/app/lib/types";
import type { Page } from "@playwright/test";

// azure_devops_capabilities (#1363): a saved policy as a saved Azure DevOps
// access profile. Authored through the editor's checklist, saved, and read
// back from what the server stored — not from what the editor posted.

function editorDialog(page: Page, title: "New policy" | "Edit policy") {
  return page.getByRole("dialog").filter({ hasText: title });
}

function policyRow(page: Page, name: string) {
  return page.getByRole("table").getByRole("row").filter({ hasText: name });
}

// "Push" alone also prefixes "Push past a branch policy", so match the label
// AND the wire name the checklist prints beside it.
function capBox(scope: ReturnType<Page["getByRole"]>, label: string, cap: string) {
  return scope.getByRole("checkbox", { name: new RegExp(`^${label}\\s*${cap}$`) });
}

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

test("a policy saved with azure_devops_capabilities reads back with the same choice", async ({ page, request }) => {
  const name = `ado-repo-policy-admin-${Date.now().toString(36)}`;
  await gotoConsole(page, "admin");
  await navTo(page, "Policies");
  await expect(page.getByRole("heading", { name: "Policies", exact: true })).toBeVisible();

  await page.getByRole("button", { name: "New policy" }).first().click();
  const dialog = editorDialog(page, "New policy");
  await expect(dialog).toBeVisible();
  await dialog.getByLabel("Name").fill(name);
  await capBox(dialog, ADO.CAP_READ, "read").click();
  await capBox(dialog, ADO.CAP_POLICY_ADMIN, "policy_admin").click();
  // The checklist writes the same document the textarea shows.
  await expect(dialog.getByLabel("Spec (JSON)")).toHaveValue(/"azure_devops_capabilities"/);

  const created = page.waitForResponse((r) => r.url().includes("/api/v1/policies") && r.request().method() === "POST");
  await dialog.getByRole("button", { name: "Create policy" }).click();
  const res = await created;
  expect(res.status()).toBe(201);
  const stored = (await res.json()) as { id: string; spec: RunPolicySpec };
  expect(stored.spec.azure_devops_capabilities).toEqual(["policy_admin", "read"]);
  await expect(dialog).toBeHidden();
  await expect(policyRow(page, name)).toBeVisible();

  // Read back what the server stored, then the row reopened in the editor.
  const got = await request.get(`/api/v1/policies/${stored.id}`, { headers: auth });
  expect(got.ok()).toBe(true);
  expect(((await got.json()) as { spec: RunPolicySpec }).spec.azure_devops_capabilities).toEqual([
    "policy_admin",
    "read",
  ]);
  await policyRow(page, name).click();
  const sheet = page.getByRole("dialog").filter({ hasText: name });
  await expect(sheet).toBeVisible();
  await sheet.getByText("View raw JSON").click();
  await expect(sheet).toContainText("azure_devops_capabilities");
  await sheet.getByRole("button", { name: "Edit policy" }).click();
  const edit = editorDialog(page, "Edit policy");
  await expect(edit).toBeVisible();
  await expect(capBox(edit, ADO.CAP_POLICY_ADMIN, "policy_admin")).toBeChecked();
  await expect(capBox(edit, ADO.CAP_READ, "read")).toBeChecked();
  await expect(capBox(edit, ADO.CAP_CODE_WRITE, "code_write")).not.toBeChecked();
  await edit.getByRole("button", { name: "Cancel" }).click();
  await expect(edit).toBeHidden();

  // Clean up through the API so the backend is left as found.
  const del = await request.delete(`/api/v1/policies/${stored.id}`, { headers: auth });
  expect(del.ok()).toBe(true);
});
