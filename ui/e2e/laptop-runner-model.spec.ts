/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, expandCard, gotoConsole, navToRoute } from "./fixtures";
import { LOCAL_MODEL_EDITOR as L } from "../src/app/lib/local-model-copy";
import { MODEL_PROVIDERS, PROVIDER_EDITOR } from "../src/app/lib/model-providers-copy";

// r12 (#2957): the local model server's laptop-runner switch in a real browser. Off by default; on, the
// disclosure and a save that sends local.allow_on_laptop_runners; a Base URL off host.docker.internal gets
// the line under it and then the real server's sentence. The not-an-admin state (r12 frame D) is
// vitest's (model-provider-laptop.test.tsx): Settings refuses anyone without the admin role, so no
// browser reaches the editor that way.

test.describe.configure({ mode: "serial" });

type Page = import("@playwright/test").Page;

async function openLocalForm(page: Page, baseURL: string) {
  await navToRoute(page, "/admin/settings");
  await expandCard(page, "Model providers");
  await page.getByRole("button", { name: "Add model provider" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: MODEL_PROVIDERS.KIND.local_model }).click();
  await dialog.getByLabel("Name", { exact: true }).fill("Laptop GPU");
  await dialog.getByLabel("Base URL").fill(baseURL);
  const models = dialog.getByPlaceholder(L.MODELS_PLACEHOLDER);
  await models.fill("qwen2.5-coder:7b");
  await models.press("Enter");
  return dialog;
}

test.describe("Settings — a local model server on people's own computers", () => {
  test("off by default; on shows what it turns on, and Save sends the flag", async ({ page }) => {
    let stored: { providers?: unknown[] } = {};
    const puts: { providers: Array<{ local?: Record<string, unknown> }> }[] = [];
    await page.route("**/api/v1/model-providers", async (route) => {
      const req = route.request();
      if (req.method() === "PUT") {
        puts.push(req.postDataJSON());
        stored = puts[0];
      }
      await route.fulfill({ status: 200, headers: { ETag: '"l1"' }, json: req.method() === "PUT" ? stored : { ...stored, connected_people: {} } });
    });
    await gotoConsole(page);
    const dialog = await openLocalForm(page, "http://host.docker.internal:11434");
    const toggle = dialog.getByRole("switch", { name: L.LAPTOP_SWITCH });
    await expect(toggle).toHaveAttribute("aria-checked", "false");
    await expect(dialog.getByText(L.LAPTOP_OFF_HINT)).toBeVisible();
    await expect(dialog.getByRole("region", { name: L.LAPTOP_DISCLOSURE })).toHaveCount(0);

    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-checked", "true");
    await expect(dialog.getByText(L.LAPTOP_ON_HINT)).toBeVisible();
    const disclosure = dialog.getByRole("region", { name: L.LAPTOP_DISCLOSURE });
    await expect(disclosure.getByRole("listitem")).toHaveText([...L.LAPTOP_BULLETS]);
    await expect(disclosure.getByText(L.LAPTOP_FOOT)).toBeVisible();

    await dialog.getByRole("button", { name: PROVIDER_EDITOR.SAVE, exact: true }).click();
    await expect(page.getByText("Provider saved.")).toBeVisible();
    expect(puts).toHaveLength(1);
    expect(puts[0].providers[0].local).toEqual({ models: ["qwen2.5-coder:7b"], allow_on_laptop_runners: true });
  });

  test("a Base URL off host.docker.internal: the line under it, then the real server's sentence in the red card", async ({ page }) => {
    await gotoConsole(page);
    const dialog = await openLocalForm(page, "http://192.168.1.20:11434");
    await expect(dialog.getByText(L.LAPTOP_BASE_URL)).toHaveCount(0);
    await dialog.getByRole("switch", { name: L.LAPTOP_SWITCH }).click();
    await expect(dialog.getByText(L.LAPTOP_BASE_URL)).toBeVisible();
    await expect(dialog.getByLabel("Base URL")).toHaveAttribute("aria-invalid", "true");

    await dialog.getByRole("button", { name: PROVIDER_EDITOR.SAVE, exact: true }).click();
    const note = dialog.getByRole("alert");
    await expect(note.getByText("This provider can't be saved as written", { exact: true })).toBeVisible();
    await expect(note.locator("p")).toContainText(
      'model_providers: "laptop-gpu": local.allow_on_laptop_runners needs base_url on host.docker.internal — the model server on the runner\'s own computer',
    );
    await expect(page.getByText("Provider saved.")).toHaveCount(0);
  });
});
