/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { MODEL_LEDE, MODEL_PROVIDERS as M } from "../src/app/lib/model-providers-copy";

// Settings → Model providers (#536, packet MP-A). GET /model-providers and GET
// /agent-providers are stubbed: this harness has no people to connect, so a
// count above 0 can only come from a stub. /setup/status is the real one — in
// legacy open mode every catalog agent is turned on, which is what A9 reads.

async function stub(page: Page, body: unknown, roster: unknown = {}, status = 200): Promise<void> {
  await page.route("**/api/v1/model-providers", (route) =>
    route.request().method() === "GET"
      ? route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) })
      : route.fallback(),
  );
  await page.route("**/api/v1/agent-providers", (route) =>
    route.request().method() === "GET"
      ? route.fulfill({ contentType: "application/json", body: JSON.stringify(roster) })
      : route.fallback(),
  );
}

async function gotoSettings(page: Page, path = "/admin/settings"): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, path);
}

const list = (page: Page) => page.getByTestId("model-providers-list");

test.describe("Settings — Model providers list", () => {
  test("A1: nothing set up is the empty state, and Add model provider opens its page", async ({ page }) => {
    await stub(page, {});
    await gotoSettings(page);
    await expect(list(page).getByRole("heading", { name: M.TITLE, exact: true })).toBeVisible();
    await expect(list(page).getByText(MODEL_LEDE)).toBeVisible();
    await expect(list(page).getByText(M.EMPTY_TITLE)).toBeVisible();
    await expect(list(page).getByText(M.EMPTY_BODY)).toBeVisible();

    await list(page).getByRole("button", { name: M.ADD_CTA }).click();
    await expect(page).toHaveURL(/\/admin\/settings\/model-providers\/new$/);
    await expect(page.getByRole("heading", { name: M.ADD_CTA, level: 1 })).toBeVisible();
  });

  test("rows carry kind, what each person provides, Used by, the default and the connected count", async ({ page }) => {
    await stub(
      page,
      {
        providers: [
          { id: "bedrock-prod", name: "Bedrock (prod)", kind: "bedrock_sso", harnesses: [{ harness: "claude-code" }] },
          { id: "anthropic-key", kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] },
        ],
        connected_people: { "bedrock-prod": 12, "anthropic-key": 0 },
      },
      { agents: [{ id: "claude-code", mechanism: "none", default_provider: "bedrock-prod" }] },
    );
    await gotoSettings(page);

    const bedrock = page.getByTestId("model-provider-bedrock-prod");
    await expect(bedrock.getByText("Bedrock (prod)")).toBeVisible();
    await expect(bedrock.getByText("Amazon Bedrock")).toBeVisible();
    await expect(bedrock.getByText(M.PROVIDES.SSO)).toBeVisible();
    await expect(bedrock.getByText("Used by Claude Code")).toBeVisible();
    await expect(bedrock.getByText("Default for Claude Code")).toBeVisible();
    await expect(bedrock.getByText("Connected by 12 people")).toBeVisible();

    const key = page.getByTestId("model-provider-anthropic-key");
    await expect(key.getByText("Anthropic API key")).toHaveCount(1);
    await expect(key.getByText(M.PROVIDES.KEY)).toBeVisible();
    await expect(key.getByText("No one has connected yet")).toBeVisible();

    // A9: Codex CLI is on (legacy open mode) and nothing serves it.
    await expect(list(page).getByText(M.HARNESS_UNSERVED("Codex CLI"))).toBeVisible();
  });

  test("a failed read offers Retry, and Retry reads again", async ({ page }) => {
    await stub(page, { error: "boom" }, {}, 500);
    await gotoSettings(page);
    await expect(list(page).getByText(M.FETCH_FAILED_TITLE)).toBeVisible();

    await page.unroute("**/api/v1/model-providers");
    await stub(page, {});
    await list(page).getByRole("button", { name: "Retry" }).click();
    await expect(list(page).getByText(M.EMPTY_TITLE)).toBeVisible();
  });

  test("the account page (User view) has no list", async ({ page }) => {
    await stub(page, {});
    await gotoSettings(page, "/account");
    await expect(page.getByRole("heading", { name: "Host", level: 3 })).toBeVisible();
    await expect(list(page)).toHaveCount(0);
  });
});
