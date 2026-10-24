/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2588 — the workspace page's Model provider row: an admin finds a workspace pinned to a provider that no
// longer lists, rebinds or clears it with Change (the existing Model access dialog, PUT /workspaces/{id}/llm-cred);
// a member who owns the workspace gets Change disabled, because the server's write is admin-only.
import type { Page } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, gotoConsole, navToRoute } from "./fixtures";
import { OPERATOR_ONLY_REASON } from "../src/app/components/wardyn/copy";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const GONE = "e2e-gone-provider";

async function pinnedWorkspace(page: Page, tag: string): Promise<{ id: string; name: string }> {
  const name = `e2e-mp-${tag}-${Date.now()}`;
  const created = await page.request.post("/api/v1/workspaces", {
    headers: auth,
    data: { name, sources: [{ type: "local_dir", path: `/tmp/${name}` }] },
  });
  expect(created.ok()).toBe(true);
  const id = ((await created.json()) as { id: string }).id;
  const pinned = await page.request.put(`/api/v1/workspaces/${id}/llm-cred`, { headers: auth, data: { provider_ref: GONE } });
  expect(pinned.ok()).toBe(true);
  return { id, name };
}


test.describe("Workspace detail — Model provider (#2588)", () => {
  test("an admin sees the dead binding's id, opens Change, clears it, and the row reads None", async ({ page }) => {
    const { id, name } = await pinnedWorkspace(page, "admin");
    await gotoConsole(page);
    await navToRoute(page, `/workspaces/${id}`);
    await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();
    await expect(page.getByText(GONE, { exact: true })).toBeVisible();

    await page.getByRole("button", { name: "Change", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: `Model access — ${name}` });
    await expect(dialog.getByText(`${GONE} (not in the Model providers list)`)).toBeVisible();
    await dialog.getByRole("radio", { name: /^None/ }).click();
    await dialog.getByRole("button", { name: "Save", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByText(GONE, { exact: true })).toHaveCount(0);

    const ws = await (await page.request.get(`/api/v1/workspaces/${id}`, { headers: auth })).json();
    expect(ws.llm_cred?.provider_ref ?? "").toBe("");
    await page.request.delete(`/api/v1/workspaces/${id}`, { headers: auth });
  });

  test("a member who owns the workspace does not get an enabled Change", async ({ page }) => {
    const { id, name } = await pinnedWorkspace(page, "owner");
    const OWNER = "alice@corp.example";
    await page.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.role = "user";
      json.operator = false;
      json.security_operator = false;
      json.principal = OWNER;
      await route.fulfill({ response, json });
    });
    await page.route(`**/api/v1/workspaces/${id}`, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const response = await route.fetch();
      const json = await response.json();
      json.owned_by = OWNER;
      await route.fulfill({ response, json });
    });
    await gotoConsole(page);
    await navToRoute(page, `/workspaces/${id}`);
    await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();

    // The owner can delete (canMutate), and still cannot change the binding (useOperator).
    await expect(page.getByRole("button", { name: "Delete this workspace" })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Change", exact: true })).toBeDisabled();
    await expect(page.locator("p.mt-1\\.5.text-xs", { hasText: OPERATOR_ONLY_REASON })).toBeVisible();
    await page.request.delete(`/api/v1/workspaces/${id}`, { headers: auth });
  });
});
