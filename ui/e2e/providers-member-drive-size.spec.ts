/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, expandCard, gotoConsole, sidebarLink } from "./fixtures";
import { MEMBER_DRIVES, PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import type { Page } from "@playwright/test";

// The Storage tab's member-drive size. On Kubernetes the server refuses an enabled
// "let people create their own drives" switch with no size, so the tab carries a
// "Size of each drive" field there; on Docker the server refuses a size, so the tab
// shows none. This backend is a Docker-style `-runner none` one: the Kubernetes
// answer is /setup/status's runner.kubernetes, spliced at the route.

async function openStorageTab(page: Page): Promise<void> {
  await gotoConsole(page, "admin");
  await sidebarLink(page, "Settings").click();
  const card = page.getByTestId("providers-card");
  await expect(card).toBeVisible();
  await expandCard(page, "Workspace providers");
  await card.getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
  await page.getByRole("tab", { name: PROVIDERS.STORAGE_TAB }).click();
  await expect(page.getByLabel(MEMBER_DRIVES.FIELD_MAX)).toBeVisible();
}

async function spliceKubernetes(page: Page): Promise<void> {
  await page.route("**/api/v1/setup/status*", async (route) => {
    const body = (await (await route.fetch()).json()) as { runner: Record<string, unknown> };
    body.runner = { ...body.runner, kubernetes: true };
    await route.fulfill({ json: body });
  });
}

test.describe("the member-drive size on the Storage tab", () => {
  test("a Docker deployment shows no size field", async ({ page }) => {
    await openStorageTab(page);
    await expect(page.getByRole("switch", { name: MEMBER_DRIVES.FIELD_ENABLED })).toBeVisible();
    await expect(page.getByLabel(MEMBER_DRIVES.FIELD_SIZE)).toHaveCount(0);
  });

  test("on Kubernetes a fresh state saves once the switch is on and a size is set", async ({ page }) => {
    await spliceKubernetes(page);
    let sent: { storage?: { user_drive?: { member_create?: Record<string, unknown> } } } | undefined;
    await page.route("**/api/v1/workspace-providers", async (route) => {
      const req = route.request();
      if (req.method() === "GET") return route.fulfill({ json: {}, headers: { etag: '"e2e-drive-1"' } });
      if (req.method() !== "PUT") return route.fallback();
      sent = req.postDataJSON();
      return route.fulfill({ json: { ...sent, sources_no_longer_admitted: 0 }, headers: { etag: '"e2e-drive-2"' } });
    });
    await openStorageTab(page);

    const size = page.getByLabel(MEMBER_DRIVES.FIELD_SIZE);
    await expect(size).toBeVisible();
    await expect(page.getByText(MEMBER_DRIVES.SIZE_HINT)).toBeVisible();
    await expect(size).toBeDisabled();
    await page.getByRole("switch", { name: MEMBER_DRIVES.FIELD_ENABLED }).click();
    await size.fill("2");
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toBeVisible();
    expect(sent?.storage?.user_drive?.member_create).toEqual({ enabled: true, max_per_person: 1, size_mib: 2048 });
  });
});
