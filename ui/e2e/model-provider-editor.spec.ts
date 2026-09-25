/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute } from "./fixtures";

// The provider editor (#537, model-provider-editor.tsx), opened from
// Settings until #536's Model providers list hosts it. Two things only an e2e
// proves: the add → save → list round trip through the real client (If-Match,
// the whole document, the toast, the re-read), and E6 rendering the REAL
// server's 400 body verbatim.

// Serial: one backend; the E6 test writes nothing (the server refuses it) but
// reads the real stored block the first test's stubs stand in front of.
test.describe.configure({ mode: "serial" });

async function addEndpoint(page: import("@playwright/test").Page, baseURL: string) {
  await page.getByRole("button", { name: "Add model provider" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "Your own endpoint" }).click();
  await dialog.getByLabel("Name").fill("Corp gateway");
  await dialog.getByLabel("Base URL").fill(baseURL);
  const paths = dialog.getByLabel("Path");
  await paths.nth(0).fill("/anthropic");
  await paths.nth(1).fill("/v1");
  await dialog.getByRole("button", { name: "Save", exact: true }).click();
  return dialog;
}

test.describe("Settings — the provider editor", () => {
  test("add → save → list: the whole document with If-Match, the toast, and the new provider on the re-read", async ({
    page,
  }) => {
    // Stubbed so the round trip leaves the shared backend's providers as it
    // found them (a stored provider would change other specs' setup status).
    let stored: { providers?: unknown[] } = {};
    let etag = '"e1"';
    const puts: { body: { providers: Array<Record<string, unknown>> }; ifMatch?: string }[] = [];
    await page.route("**/api/v1/model-providers", async (route) => {
      const req = route.request();
      if (req.method() === "PUT") {
        puts.push({ body: req.postDataJSON(), ifMatch: req.headers()["if-match"] });
        stored = puts[0].body;
        etag = '"e2"';
      }
      await route.fulfill({
        status: 200,
        headers: { ETag: etag },
        json: req.method() === "PUT" ? stored : { ...stored, connected_people: {} },
      });
    });

    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    const dialog = await addEndpoint(page, "https://gateway.corp.example");

    await expect(page.getByText("Provider saved.")).toBeVisible();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Corp gateway/ })).toBeVisible();
    expect(puts).toHaveLength(1);
    expect(puts[0].ifMatch).toBe('"e1"');
    expect(puts[0].body.providers).toEqual([
      expect.objectContaining({
        id: "corp-gateway",
        name: "Corp gateway",
        kind: "custom_endpoint",
        base_url: "https://gateway.corp.example",
        auth: { header: "Authorization", format: "Bearer %s" },
        harnesses: [
          { harness: "claude-code", path: "/anthropic" },
          { harness: "codex-cli", path: "/v1" },
        ],
      }),
    ]);
  });

  test("E6: the real server's 400 renders verbatim under the heading, and the editor keeps what was typed", async ({
    page,
  }) => {
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    const dialog = await addEndpoint(page, "http://gateway.corp.example");

    const note = dialog.getByRole("alert");
    await expect(note.getByText("This provider can't be saved as written", { exact: true })).toBeVisible();
    await expect(note.locator("p")).toHaveText(
      'invalid model providers: model_providers: "corp-gateway": base_url: must be https:// (got "http://gateway.corp.example")',
    );
    await expect(dialog.getByLabel("Base URL")).toHaveValue("http://gateway.corp.example");
    await expect(page.getByText("Provider saved.")).toHaveCount(0);
  });
});
