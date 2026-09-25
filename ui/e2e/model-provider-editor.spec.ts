/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute } from "./fixtures";

// The provider editor (#537, model-provider-editor.tsx), opened from
// Settings → Model providers (#536). Two things only an e2e
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

// #538 (MP-19b): the two kinds #537 didn't build. Same stubbing shape as
// above — the round trip through the real client is what only an e2e proves;
// every state's copy and field rules are vitest's (model-provider-editor.test.tsx).
test.describe("Settings — the provider editor: Bedrock and Claude subscription (#538)", () => {
  test("Bedrock SSO: add → save writes the bedrock block; edit reopens it prefilled and a changed field saves again", async ({
    page,
  }) => {
    let stored: { providers?: Record<string, unknown>[] } = {};
    let etag = '"b1"';
    const puts: { body: { providers: Array<Record<string, unknown>> } }[] = [];
    await page.route("**/api/v1/model-providers", async (route) => {
      const req = route.request();
      if (req.method() === "PUT") {
        puts.push({ body: req.postDataJSON() });
        stored = puts[puts.length - 1].body;
        etag = `"b${puts.length + 1}"`;
      }
      await route.fulfill({
        status: 200,
        headers: { ETag: etag },
        json: req.method() === "PUT" ? stored : { ...stored, connected_people: {} },
      });
    });

    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");

    await page.getByRole("button", { name: "Add model provider" }).click();
    let dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Amazon Bedrock" }).click();
    await dialog.getByLabel("Name", { exact: true }).fill("Bedrock (prod)");
    await dialog.getByLabel("Region").fill("us-east-1");
    await dialog.getByLabel("AWS access portal start URL").fill("https://acme.awsapps.com/start");
    await dialog.getByLabel("Pinned AWS account id").fill("111122223333");
    await dialog.getByLabel("Pinned IAM role name").fill("WardynBedrockUser");
    await dialog.getByLabel("Model").fill("acme.claude-sonnet");
    await dialog.getByRole("button", { name: "Save", exact: true }).click();

    await expect(page.getByText("Provider saved.")).toBeVisible();
    expect(puts).toHaveLength(1);
    expect(puts[0].body.providers).toEqual([
      expect.objectContaining({
        id: "bedrock-prod",
        name: "Bedrock (prod)",
        kind: "bedrock_sso",
        bedrock: {
          region: "us-east-1",
          sso_start_url: "https://acme.awsapps.com/start",
          sso_account_id: "111122223333",
          sso_role_name: "WardynBedrockUser",
        },
        harnesses: [{ harness: "claude-code", model: "acme.claude-sonnet" }],
      }),
    ]);

    // Edit: the row reopens the editor prefilled; changing Region asks no
    // confirm (nobody has connected yet) and saves again.
    await page.getByRole("button", { name: /^Bedrock \(prod\)/ }).click();
    dialog = page.getByRole("dialog");
    await expect(dialog.getByLabel("Region")).toHaveValue("us-east-1");
    await expect(dialog.getByLabel("AWS access portal start URL")).toHaveValue("https://acme.awsapps.com/start");
    await dialog.getByLabel("Region").fill("eu-west-1");
    await dialog.getByRole("button", { name: "Save", exact: true }).click();

    await expect(page.getByText("Provider saved.")).toBeVisible();
    expect(puts).toHaveLength(2);
    expect(puts[1].body.providers[0]).toMatchObject({
      id: "bedrock-prod",
      bedrock: expect.objectContaining({ region: "eu-west-1" }),
    });
  });

  test("Claude subscription: add → save writes the kind with no bedrock or auth block; edit reopens it", async ({
    page,
  }) => {
    // This harness may not have the Claude sign-in image built — splice the
    // real /setup/status so E4's kind-step option isn't disabled (QB-5).
    let cachedStatus: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cachedStatus) {
        const json = (await (await route.fetch()).json()) as Record<string, unknown>;
        const checks = ((json.checks as Array<Record<string, unknown>>) ?? []).filter(
          (c) => c.id !== "claude_signin_image",
        );
        checks.push({ id: "claude_signin_image", label: "Claude sign-in image", status: "info", detail: "resolved (e2e stub)" });
        cachedStatus = { ...json, checks };
      }
      await route.fulfill({ json: cachedStatus! });
    });

    let stored: { providers?: Record<string, unknown>[] } = {};
    let etag = '"s1"';
    const puts: { body: { providers: Array<Record<string, unknown>> } }[] = [];
    await page.route("**/api/v1/model-providers", async (route) => {
      const req = route.request();
      if (req.method() === "PUT") {
        puts.push({ body: req.postDataJSON() });
        stored = puts[puts.length - 1].body;
        etag = `"s${puts.length + 1}"`;
      }
      await route.fulfill({
        status: 200,
        headers: { ETag: etag },
        json: req.method() === "PUT" ? stored : { ...stored, connected_people: {} },
      });
    });

    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");

    await page.getByRole("button", { name: "Add model provider" }).click();
    let dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Claude subscription" }).click();
    await dialog.getByRole("button", { name: "Save", exact: true }).click();

    await expect(page.getByText("Provider saved.")).toBeVisible();
    expect(puts).toHaveLength(1);
    expect(puts[0].body.providers).toEqual([
      expect.objectContaining({
        id: "claude-subscription",
        name: "Claude subscription",
        kind: "anthropic_subscription",
        harnesses: [{ harness: "claude-code" }],
      }),
    ]);

    await page.getByRole("button", { name: /^Claude subscription/ }).click();
    dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("heading", { name: "Claude subscription" })).toBeVisible();
  });
});
