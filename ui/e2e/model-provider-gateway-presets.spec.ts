/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, expandCard, gotoConsole, navToRoute } from "./fixtures";
import { PROVIDER_EDITOR } from "../src/app/lib/model-providers-copy";

// A new Your own endpoint provider starts from a gateway preset (LiteLLM, Portkey, OpenRouter): the press fills
// the fields it names and nothing else, every field stays editable, and the save carries extra headers and no
// preset. The save route is stubbed, so the shared backend's providers are left as they were found.

type Doc = { providers?: Array<Record<string, unknown>> };

async function stubProviders(page: Page, opts: { stored?: Doc; refuse?: string } = {}) {
  let stored: Doc = opts.stored ?? {};
  const puts: Doc[] = [];
  await page.route("**/api/v1/model-providers", async (route) => {
    const req = route.request();
    if (req.method() === "PUT") {
      if (opts.refuse) {
        await route.fulfill({ status: 400, contentType: "text/plain", body: opts.refuse });
        return;
      }
      puts.push(req.postDataJSON());
      stored = puts[puts.length - 1];
    }
    await route.fulfill({
      status: 200,
      headers: { ETag: '"p1"' },
      json: req.method() === "PUT" ? stored : { ...stored, connected_people: {} },
    });
  });
  return puts;
}

async function openEndpointForm(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/admin/settings");
  await expandCard(page, "Model providers");
  await page.getByRole("button", { name: "Add model provider" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "Your own endpoint" }).click();
  return dialog;
}

const chip = (dialog: ReturnType<Page["getByRole"]>, name: string) => dialog.getByRole("radio", { name, exact: true });

test.describe("Settings — gateway presets on a new provider", () => {
  test("each preset fills exactly what it names", async ({ page }) => {
    await stubProviders(page);
    const dialog = await openEndpointForm(page);
    const paths = dialog.getByLabel("Path", { exact: true });
    const names = dialog.getByPlaceholder(PROVIDER_EDITOR.HEADER_NAME_PLACEHOLDER);

    await chip(dialog, "LiteLLM").click();
    await expect(chip(dialog, "LiteLLM")).toHaveAttribute("aria-checked", "true");
    await expect(paths.nth(0)).toHaveValue("/");
    await expect(paths.nth(1)).toHaveValue("/v1");
    await expect(dialog.getByLabel("Base URL")).toHaveValue("");
    await expect(dialog.getByLabel("Name", { exact: true })).toHaveValue("Your own endpoint");

    await chip(dialog, "Portkey self-hosted").click();
    await expect(dialog.getByRole("checkbox", { name: "Claude Code" })).toBeChecked();
    await expect(dialog.getByRole("checkbox", { name: "Codex CLI" })).not.toBeChecked();
    await expect(names).toHaveCount(1);
    await expect(names).toHaveValue("x-portkey-provider");
    await expect(dialog.getByPlaceholder(PROVIDER_EDITOR.HEADER_VALUE_PLACEHOLDER)).toHaveValue("anthropic");

    await chip(dialog, "Portkey hosted").click();
    await expect(dialog.getByLabel("Auth header")).toHaveValue("x-portkey-api-key");
    await expect(dialog.getByLabel("Value format")).toHaveValue("{token}");
    await expect(names).toHaveCount(1);
    await expect(dialog.getByPlaceholder("@<slug>")).toHaveValue("");
    await expect(dialog.getByText(PROVIDER_EDITOR.PORTKEY_SLUG_HINT)).toBeVisible();

    await chip(dialog, "OpenRouter").click();
    await expect(dialog.getByLabel("Base URL")).toHaveValue("https://openrouter.ai/api");
    // Portkey self-hosted's untick stays, as every earlier fill does; the Codex CLI Model shows once it is ticked.
    const codex = dialog.getByRole("checkbox", { name: "Codex CLI" });
    await expect(codex).not.toBeChecked();
    await codex.check();
    await expect(dialog.getByPlaceholder("openai/…")).toHaveCount(1);
    await expect(dialog.getByText(PROVIDER_EDITOR.OPENROUTER_MODEL_HINT)).toBeVisible();
    await expect(dialog.getByText(PROVIDER_EDITOR.PORTKEY_SLUG_HINT)).toBeVisible();

    await chip(dialog, "Other").click();
    await expect(dialog.getByPlaceholder("openai/…")).toHaveCount(0);
    await expect(dialog.getByLabel("Base URL")).toHaveValue("https://openrouter.ai/api");
  });

  test("after LiteLLM, the Claude Code path edits to /anthropic and stays", async ({ page }) => {
    await stubProviders(page);
    const dialog = await openEndpointForm(page);
    await chip(dialog, "LiteLLM").click();
    const claudePath = dialog.getByLabel("Path", { exact: true }).nth(0);
    await claudePath.fill("/anthropic");
    await expect(claudePath).toHaveValue("/anthropic");
    await expect(chip(dialog, "LiteLLM")).toHaveAttribute("aria-checked", "true");
  });

  test("the save carries extra_headers as name and value only, and no preset", async ({ page }) => {
    const puts = await stubProviders(page);
    const dialog = await openEndpointForm(page);
    await chip(dialog, "Portkey hosted").click();
    await dialog.getByLabel("Base URL").fill("https://gateway.corp.example");
    const paths = dialog.getByLabel("Path", { exact: true });
    await paths.nth(0).fill("/");
    await paths.nth(1).fill("/v1");
    await dialog.getByPlaceholder("@<slug>").fill("@team");
    await dialog.getByRole("button", { name: PROVIDER_EDITOR.SAVE, exact: true }).click();

    await expect.poll(() => puts.length).toBe(1);
    const sent = puts[0].providers![0];
    expect(sent.extra_headers).toEqual([{ name: "x-portkey-provider", value: "@team" }]);
    expect(sent.auth).toEqual({ header: "x-portkey-api-key", format: "%s" });
    expect(JSON.stringify(sent)).not.toMatch(/placeholder|preset|portkey_hosted/i);
  });

  test("a refused save shows the server's sentence verbatim, and the rows stay", async ({ page }) => {
    const body =
      'invalid model providers: model_providers: "portkey": extra_headers: "X-Portkey-Api-Key" is this provider\'s own credential header';
    await stubProviders(page, { refuse: body });
    const dialog = await openEndpointForm(page);
    await dialog.getByLabel("Base URL").fill("https://gateway.corp.example");
    const paths = dialog.getByLabel("Path", { exact: true });
    await paths.nth(0).fill("/");
    await paths.nth(1).fill("/v1");
    await dialog.getByRole("button", { name: PROVIDER_EDITOR.HEADERS_ADD }).click();
    await dialog.getByPlaceholder(PROVIDER_EDITOR.HEADER_NAME_PLACEHOLDER).fill("x-portkey-api-key");
    await dialog.getByPlaceholder(PROVIDER_EDITOR.HEADER_VALUE_PLACEHOLDER).fill("team");
    await dialog.getByRole("button", { name: PROVIDER_EDITOR.SAVE, exact: true }).click();

    const note = dialog.getByRole("alert");
    await expect(note.getByText("This provider can't be saved as written", { exact: true })).toBeVisible();
    await expect(note.locator("p")).toHaveText(body);
    await expect(dialog.getByPlaceholder(PROVIDER_EDITOR.HEADER_NAME_PLACEHOLDER)).toHaveValue("x-portkey-api-key");
  });

  test("editing a saved provider shows no preset row, and its headers read in canonical case", async ({ page }) => {
    await stubProviders(page, {
      stored: {
        providers: [
          {
            id: "portkey",
            uid: "u-1",
            name: "Portkey",
            kind: "custom_endpoint",
            base_url: "https://gateway.corp.example",
            auth: { header: "Authorization", format: "Bearer %s" },
            extra_headers: [{ name: "X-Portkey-Provider", value: "anthropic" }],
            harnesses: [{ harness: "claude-code", path: "/" }],
          },
        ],
      },
    });
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await expandCard(page, "Model providers");
    await page.getByRole("button", { name: /^Portkey/ }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByLabel("Base URL")).toHaveValue("https://gateway.corp.example");
    await expect(dialog.getByRole("radiogroup", { name: PROVIDER_EDITOR.PRESET_LABEL })).toHaveCount(0);
    await expect(dialog.getByPlaceholder(PROVIDER_EDITOR.HEADER_NAME_PLACEHOLDER)).toHaveValue("X-Portkey-Provider");
  });

  test("an Anthropic API key provider shows no header rows", async ({ page }) => {
    await stubProviders(page);
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await expandCard(page, "Model providers");
    await page.getByRole("button", { name: "Add model provider" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Anthropic API key" }).click();
    await expect(dialog.getByRole("button", { name: PROVIDER_EDITOR.HEADERS_ADD })).toHaveCount(0);
    await expect(dialog.getByText(PROVIDER_EDITOR.HEADERS_LABEL)).toHaveCount(0);
    await expect(dialog.getByRole("radiogroup", { name: PROVIDER_EDITOR.PRESET_LABEL })).toHaveCount(0);
  });
});
