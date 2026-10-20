/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, expandCard, gotoConsole, navTo, navToRoute } from "./fixtures";
import { LOCAL_MODEL_CARD, LOCAL_MODEL_EDITOR, LOCAL_MODEL_LIST } from "../src/app/lib/local-model-copy";
import { MODEL_PROVIDERS } from "../src/app/lib/model-providers-copy";

// M9: registering a local model server (the whole add → save → list round trip through the real
// client), the real server's refusal of a write in the editor's red card, and the run Overview's
// "Local models" card. The rules themselves are the Go validator's; what only a browser proves is
// that the sentence it answers reaches the admin whole.

test.describe.configure({ mode: "serial" });

async function openLocalForm(page: import("@playwright/test").Page, baseURL: string) {
  await navToRoute(page, "/admin/settings");
  await expandCard(page, "Model providers");
  await page.getByRole("button", { name: "Add model provider" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: MODEL_PROVIDERS.KIND.local_model }).click();
  await dialog.getByLabel("Name", { exact: true }).fill("Laptop GPU");
  await dialog.getByLabel("Base URL").fill(baseURL);
  const models = dialog.getByPlaceholder(LOCAL_MODEL_EDITOR.MODELS_PLACEHOLDER);
  await models.fill("qwen2.5-coder:7b");
  await models.press("Enter");
  await models.fill("llama3.2:3b");
  await models.press("Enter");
  return dialog;
}

test.describe("Settings — a local model server", () => {
  test("add → save → list: the whole record with If-Match, then the server's address and model count", async ({ page }) => {
    // Stubbed, so the shared backend's providers are left as found.
    let stored: { providers?: unknown[] } = {};
    let etag = '"l1"';
    const puts: { body: { providers: Array<Record<string, unknown>> }; ifMatch?: string }[] = [];
    await page.route("**/api/v1/model-providers", async (route) => {
      const req = route.request();
      if (req.method() === "PUT") {
        puts.push({ body: req.postDataJSON(), ifMatch: req.headers()["if-match"] });
        stored = puts[0].body;
        etag = '"l2"';
      }
      await route.fulfill({
        status: 200,
        headers: { ETag: etag },
        json: req.method() === "PUT" ? stored : { ...stored, connected_people: {} },
      });
    });

    await gotoConsole(page);
    const dialog = await openLocalForm(page, "http://host.docker.internal:11434");
    await expect(dialog.getByText(LOCAL_MODEL_EDITOR.MODELS_COUNT(2))).toBeVisible();
    await dialog.getByLabel(LOCAL_MODEL_EDITOR.REPLY).fill("4096");
    await dialog.getByRole("button", { name: "Save", exact: true }).click();

    await expect(page.getByText("Provider saved.")).toBeVisible();
    await expect(dialog).toHaveCount(0);
    expect(puts).toHaveLength(1);
    expect(puts[0].ifMatch).toBe('"l1"');
    expect(puts[0].body.providers).toEqual([
      expect.objectContaining({
        id: "laptop-gpu",
        name: "Laptop GPU",
        kind: "local_model",
        base_url: "http://host.docker.internal:11434",
        local: { models: ["qwen2.5-coder:7b", "llama3.2:3b"], max_output: 4096 },
        harnesses: [
          { harness: "claude-code", model: "qwen2.5-coder:7b" },
          { harness: "codex-cli", model: "qwen2.5-coder:7b" },
        ],
      }),
    ]);
    const row = page.getByTestId("model-provider-laptop-gpu");
    await expect(row.getByText(LOCAL_MODEL_LIST.ADDRESS("host.docker.internal:11434", 2))).toBeVisible();
    await expect(row.getByText(LOCAL_MODEL_LIST.CHIP_NO_TOKEN)).toBeVisible();
  });

  test("a refused write: the real server's sentence in the red card, the Base URL outlined, what was typed kept", async ({ page }) => {
    await gotoConsole(page);
    const dialog = await openLocalForm(page, "http://localhost:11434");
    await dialog.getByRole("button", { name: "Save", exact: true }).click();

    const note = dialog.getByRole("alert");
    await expect(note.getByText("This provider can't be saved as written", { exact: true })).toBeVisible();
    await expect(note.locator("p")).toContainText(
      'model_providers: "laptop-gpu": base_url "http://localhost:11434" names the proxy\'s own loopback, not the machine the model server runs on — use host.docker.internal on a laptop, or the node\'s address',
    );
    await expect(dialog.getByLabel("Base URL")).toHaveAttribute("aria-invalid", "true");
    await expect(dialog.getByLabel("Base URL")).toHaveValue("http://localhost:11434");
    await expect(dialog.getByText("qwen2.5-coder:7b", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("Provider saved.")).toHaveCount(0);
  });
});

test.describe("Run detail — the Local models card", () => {
  test("a run on a local model server shows its allowed models, the server and the caps", async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.route("**/api/v1/model-providers", (route) =>
      route.request().method() !== "GET"
        ? route.fallback()
        : route.fulfill({
            status: 200,
            headers: { ETag: '"c1"' },
            json: {
              providers: [
                {
                  id: "laptop-gpu",
                  name: "Laptop GPU",
                  kind: "local_model",
                  base_url: "http://host.docker.internal:11434",
                  local: { models: ["qwen2.5-coder:7b", "llama3.2:3b"] },
                  harnesses: [
                    { harness: "claude-code", model: "qwen2.5-coder:7b" },
                    { harness: "codex-cli", model: "qwen2.5-coder:7b" },
                  ],
                },
              ],
              connected_people: {},
              entra_login_configured: false,
              kinds_available: ["local_model"],
            },
          }),
    );
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 2") Object.assign(json, { model_provider_id: "laptop-gpu", model_provider_name: "Laptop GPU" });
      await route.fulfill({ response, json });
    });
    // The kind is not on the run row; the page reads it off the run.create audit row.
    await page.route("**/api/v1/audit*", async (route) => {
      const url = new URL(route.request().url());
      const response = await route.fetch();
      if (!url.searchParams.get("run_id") || url.searchParams.get("action") || url.searchParams.get("action_prefix")) {
        return route.fulfill({ response });
      }
      const rows = (await response.json()) as { action: string; data?: Record<string, unknown> }[];
      const snapshot = { id: "laptop-gpu", kind: "local_model", name: "Laptop GPU" };
      await route.fulfill({
        response,
        json: rows.map((r) => (r.action === "run.create" ? { ...r, data: { ...r.data, model_provider: snapshot } } : r)),
      });
    });

    await gotoConsole(page);
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 2").click();

    const card = page.locator("section", { has: page.getByRole("heading", { name: LOCAL_MODEL_CARD.TITLE }) });
    await expect(card).toBeVisible();
    await expect(card.getByText(LOCAL_MODEL_CARD.LEDE)).toBeVisible();
    await expect(card.getByText("qwen2.5-coder:7b")).toBeVisible();
    await expect(card.getByText(LOCAL_MODEL_CARD.ASKED_FOR(["Claude Code", "Codex CLI"]))).toBeVisible();
    await expect(card.getByText(LOCAL_MODEL_CARD.NOT_ASKED_FOR)).toBeVisible();
    await expect(card.getByText("host.docker.internal:11434")).toBeVisible();
    await expect(card.getByText("Up to 30 minutes")).toBeVisible();
    await expect(card.getByText(LOCAL_MODEL_CARD.NO_MANAGEMENT)).toBeVisible();
  });
});
