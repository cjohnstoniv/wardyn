/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, navTo } from "./fixtures";
import { ADO_ACCESS } from "../src/app/lib/ado-access-copy";
import { ADO_ENTRA_EDITOR } from "../src/app/lib/workspace-providers-copy";
import type { RunPolicySpec } from "../src/app/lib/types";
import type { Locator, Page } from "@playwright/test";

// azure_devops_capabilities (#1363): a saved policy as a saved Azure DevOps
// access profile — the approved mock's "Azure DevOps access" section (Member ·
// State 4) and New Run's summary line (State 5). The hermetic backend runs no
// Entra tenant, so the row's ceiling is spliced into /setup/status.scm_access,
// the one field the editor reads it from; everything else is the daemon's own.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const CEILING = ["read", "code_write", "pr", "policy_admin"];

async function mockCeiling(page: Page): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const json = await (await route.fetch()).json();
      json.scm_access = { state: "live", source: "org", org: "https://dev.azure.com/acme", kind: "azure_devops",
        capability_ceiling: CEILING };
      cached = json;
    }
    await route.fulfill({ json: cached! });
  });
}

function editorDialog(page: Page, title: "New policy" | "Edit policy") {
  return page.getByRole("dialog").filter({ hasText: title });
}

function policyRow(page: Page, name: string) {
  return page.getByRole("table").getByRole("row").filter({ hasText: name });
}

function capBox(scope: Locator, name: string) {
  return scope.getByRole("checkbox", { name: new RegExp(`^${name}`) });
}

const SPEC: RunPolicySpec = { allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC2" };

test("the Azure DevOps access section locks what the ceiling does not grant, and saves what it does", async ({ page, request }) => {
  const name = `ado-repo-policy-admin-${Date.now().toString(36)}`;
  await mockCeiling(page);
  await gotoConsole(page, "admin");
  await navTo(page, "Policies");
  await expect(page.getByRole("heading", { name: "Policies", exact: true })).toBeVisible();

  await page.getByRole("button", { name: "New policy" }).first().click();
  const dialog = editorDialog(page, "New policy");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText(ADO_ACCESS.SECTION_LEAD)).toBeVisible();
  await expect(dialog.getByText(ADO_ACCESS.HIGH_RISK_WARN_MEMBER)).toBeVisible();

  // Off the ceiling: disabled, and it says why on hover.
  const locked = capBox(dialog, "Manage service connections");
  await expect(locked).toBeDisabled();
  await expect(dialog.getByTitle(ADO_ACCESS.LOCKED).filter({ hasText: "Manage service connections" })).toBeVisible();
  await expect(dialog.getByTitle(ADO_ACCESS.LOCKED)).toHaveCount(14 - CEILING.length);
  await expect(capBox(dialog, "Change branch policies")).toBeEnabled();

  await dialog.getByLabel("Name").fill(name);
  await capBox(dialog, "Read").click();
  await capBox(dialog, "Change branch policies").click();
  await expect(dialog.getByLabel("Spec (JSON)")).toHaveValue(/"azure_devops_capabilities"/);

  const created = page.waitForResponse((r) => r.url().includes("/api/v1/policies") && r.request().method() === "POST");
  await dialog.getByRole("button", { name: "Create policy" }).click();
  const res = await created;
  expect(res.status()).toBe(201);
  const stored = (await res.json()) as { id: string; spec: RunPolicySpec };
  try {
    expect(stored.spec.azure_devops_capabilities).toEqual(["policy_admin", "read"]);
    await expect(dialog).toBeHidden();

    // The list names what it grants (the mock's second example policy).
    const summary = policyRow(page, name).getByTestId("ado-access-summary");
    await expect(summary).toHaveText(/^Azure DevOps: Read · Change branch policies\s*High risk$/);

    // Read back what the server stored, then reopen it in the editor.
    const got = await request.get(`/api/v1/policies/${stored.id}`, { headers: auth });
    expect(((await got.json()) as { spec: RunPolicySpec }).spec.azure_devops_capabilities).toEqual(["policy_admin", "read"]);
    await policyRow(page, name).click();
    const sheet = page.getByRole("dialog").filter({ hasText: name });
    await sheet.getByRole("button", { name: "Edit policy" }).click();
    const edit = editorDialog(page, "Edit policy");
    await expect(edit).toBeVisible();
    await expect(capBox(edit, "Change branch policies")).toBeChecked();
    await expect(capBox(edit, "Read")).toBeChecked();
    await expect(capBox(edit, "Push to the run's own branch")).not.toBeChecked();
    await edit.getByRole("button", { name: "Cancel" }).click();
    await expect(edit).toBeHidden();
  } finally {
    expect((await request.delete(`/api/v1/policies/${stored.id}`, { headers: auth })).ok()).toBe(true);
  }
});

test("New Run summarises the picked saved policy's Azure DevOps access", async ({ page, request }) => {
  const stamp = Date.now().toString(36);
  const ids: string[] = [];
  try {
    for (const [name, caps] of [
      [`ADO contributor ${stamp}`, ["read", "code_write", "pr"]],
      [`ADO repo-policy admin ${stamp}`, ["read", "policy_admin"]],
    ] as const) {
      const res = await request.post("/api/v1/policies", {
        headers: auth,
        data: { name, spec: { ...SPEC, azure_devops_capabilities: caps } },
      });
      expect(res.status()).toBe(201);
      ids.push(((await res.json()) as { id: string }).id);
    }
    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await page.getByRole("button", { name: /^Reuse a saved policy/ }).click();
    const summary = page.getByTestId("ado-access-summary");

    await page.getByRole("combobox", { name: "Saved policy" }).click();
    await page.getByRole("option", { name: `ADO contributor ${stamp}` }).click();
    await expect(summary).toHaveText("Azure DevOps: Read · Contribute");

    await page.getByRole("combobox", { name: "Saved policy" }).click();
    await page.getByRole("option", { name: `ADO repo-policy admin ${stamp}` }).click();
    await expect(summary).toHaveText(/^Azure DevOps: Read · Change branch policies\s*High risk$/);
    await expect(summary.getByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toBeVisible();
  } finally {
    for (const id of ids) expect((await request.delete(`/api/v1/policies/${id}`, { headers: auth })).ok()).toBe(true);
  }
});
