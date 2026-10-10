/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */
import { test, expect, expandCard, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";
import { CONNECTIONS } from "../src/app/components/wardyn/copy/door";

const provider = {
  id: "gateway", name: "Gateway", kind: "custom_endpoint",
  harnesses: ["claude-code"], default_for: ["claude-code"], host: "new.example",
};

test("connection changes disclose the destination and store failures re-check without replacing a credential", async ({ page }) => {
  await mockMemberRole(page);
  let cause = "store_unreadable";
  let checks = 0;
  let writes = 0;
  let cached: Record<string, unknown> | undefined;
  await page.route("**/api/v1/setup/status*", async (route) => {
    cached ??= await (await route.fetch()).json() as Record<string, unknown>;
    checks++;
    await route.fulfill({ json: {
      ...cached, model_providers: [provider],
      provider_access: [{ provider: provider.id, state: "not_configured", cause, new_destination: provider.host }],
    } });
  });
  await page.route("**/api/v1/model-providers/gateway/credential", async (route) => {
    writes++;
    await route.fulfill({ status: 204 });
  });
  await gotoConsole(page);
  await navToRoute(page, "/account");
  await expandCard(page, CONNECTIONS.TITLE);
  const card = page.getByTestId("model-connections-card");
  await expect(card).toContainText(CONNECTIONS.STORE_UNREADABLE);
  const before = checks;
  await card.getByRole("button", { name: CONNECTIONS.RECHECK, exact: true }).click();
  await expect.poll(() => checks).toBeGreaterThan(before);
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(writes).toBe(0);
  cause = "destination_changed";
  await card.getByRole("button", { name: CONNECTIONS.RECHECK, exact: true }).click();
  await expect(card).toContainText(CONNECTIONS.DESTINATION_CHANGED(provider.host));
  await card.getByRole("button", { name: CONNECTIONS.REVIEW_RECONNECT }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText(CONNECTIONS.DESTINATION_CHANGED(provider.host));
  await expect(dialog.locator("input")).toHaveValue("");
  expect(writes).toBe(0);
});
