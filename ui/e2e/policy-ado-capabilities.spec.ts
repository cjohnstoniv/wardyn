/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, goToNewRunPanel } from "./fixtures";
import { ADO_ACCESS } from "../src/app/lib/ado-access-copy";
import { ADO_ENTRA_EDITOR } from "../src/app/lib/workspace-providers-copy";
import type { RunPolicySpec } from "../src/app/lib/types";
import type { Locator, Page } from "@playwright/test";
import { SPEC_LABEL } from "./policy-source";

// azure_devops_capabilities (#1363): a saved policy as a saved Azure DevOps
// access profile — the approved mock's "Azure DevOps access" section (Member ·
// State 4) and New Run's summary line (State 5). The hermetic backend runs no
// Entra tenant, so the row's ceiling is spliced into /setup/status.scm_access,
// the one field the editor reads it from; everything else is the daemon's own.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
// The per-area packet's row (Member · State 3), as its locks show it.
const CEILING = [
  "code_read", "code_write", "pr", "policy_admin", "work_read", "work_write", "wiki_read",
  "build_read", "packaging_read", "project_read",
];
const ELEVEN_READS = [
  "code_read", "work_read", "wiki_read", "build_read", "release_read", "serviceendpoint_read",
  "library_read", "packaging_read", "test_read", "project_read", "identity_read",
];

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
  await expect(dialog.getByTitle(ADO_ACCESS.LOCKED)).toHaveCount(28 - CEILING.length);
  await expect(capBox(dialog, "Edit branch policies")).toBeEnabled();

  await dialog.getByLabel("Name", { exact: true }).fill(name);
  await capBox(dialog, "Read code").click();
  await capBox(dialog, "Edit branch policies").click();
  await expect(dialog.getByLabel(SPEC_LABEL)).toHaveValue(/azure_devops_capabilities:/);

  const created = page.waitForResponse((r) => r.url().includes("/api/v1/policies") && r.request().method() === "POST");
  await dialog.getByRole("button", { name: "Create policy" }).click();
  const res = await created;
  expect(res.status()).toBe(201);
  const stored = (await res.json()) as { id: string; spec: RunPolicySpec };
  try {
    expect(stored.spec.azure_devops_capabilities).toEqual(["code_read", "policy_admin"]);
    await expect(dialog).toBeHidden();

    // The list names what it grants (the per-area packet's third example policy).
    const summary = policyRow(page, name).getByTestId("ado-access-summary");
    await expect(summary).toHaveText(/^Azure DevOps: Read code · Edit branch policies\s*High risk$/);

    // Read back what the server stored, then reopen it in the editor.
    const got = await request.get(`/api/v1/policies/${stored.id}`, { headers: auth });
    expect(((await got.json()) as { spec: RunPolicySpec }).spec.azure_devops_capabilities).toEqual(["code_read", "policy_admin"]);
    await policyRow(page, name).click();
    const sheet = page.getByRole("dialog").filter({ hasText: name });
    await sheet.getByRole("button", { name: "Edit policy" }).click();
    const edit = editorDialog(page, "Edit policy");
    await expect(edit).toBeVisible();
    await expect(capBox(edit, "Edit branch policies")).toBeChecked();
    await expect(capBox(edit, "Read code")).toBeChecked();
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
  // The per-area packet's four saved policies (Member · State 4), with the line each reads as.
  const policies: [string, string[], string | RegExp][] = [
    [`ADO contributor ${stamp}`, [...ELEVEN_READS, "code_write", "pr"], "Azure DevOps: Read (every area) · Repos"],
    [`ADO ticket triage ${stamp}`, ["code_read", "work_read", "work_write", "project_read"],
      "Azure DevOps: Read code · Boards · View projects & teams"],
    [`ADO repo-policy admin ${stamp}`, ["code_read", "policy_admin"], /^Azure DevOps: Read code · Edit branch policies\s*High risk$/],
    [`ADO backlog cleanup ${stamp}`, ["work_read", "work_write", "work_admin"],
      /^Azure DevOps: Boards · Delete work items & manage work tracking\s*High risk$/],
  ];
  try {
    for (const [name, caps] of policies) {
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
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: /^Reuse a saved policy/ }).click();
    const summary = page.getByTestId("ado-access-summary");

    for (const [name, , line] of policies) {
      await goToNewRunPanel(page, "policy");
      await page.getByRole("combobox", { name: "Saved policy" }).click();
      await page.getByRole("option", { name }).click();
      await goToNewRunPanel(page, "access");
      await expect(summary).toHaveText(line);
    }
    await expect(summary.getByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toBeVisible();
  } finally {
    for (const id of ids) expect((await request.delete(`/api/v1/policies/${id}`, { headers: auth })).ok()).toBe(true);
  }
});

test("the pre-split read id is refused at the policy door", async ({ request }) => {
  const res = await request.post("/api/v1/policies", {
    headers: auth,
    data: { name: `ado-pre-split-${Date.now().toString(36)}`, spec: { ...SPEC, azure_devops_capabilities: ["read"] } },
  });
  expect(res.status()).toBe(400);
  expect(((await res.json()) as { reason: string }).reason).toBe("ado_capability_unknown");
});
