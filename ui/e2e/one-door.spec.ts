/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, Route } from "@playwright/test";
import { test, expect, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";
import { attachModeFrame, stubAttachSocket, stubAttachTicket } from "./attach-stub";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { BANNER, CLAUDE_DOOR, KEY_DOOR } from "../src/app/components/wardyn/copy/door";
import { AGENTS, PROVIDERS } from "../src/app/lib/workspace-providers-copy";

// ONE door (#544, packet MP-E §5.9; the strip that opens it, #540): Settings,
// the Agents tab, Your model connections (#541 — Getting Started's own
// button, replaced) and the strip keep their buttons, and every one of them
// opens the single dialog the shell mounts — keyed by the provider it is for.
// Nothing on a route mounts a sign-in pane of its own any more, so leaving
// the route never takes the door (or its sign-in sandbox) with it.
//
// Hermetic for signin-door-aws.spec.ts's reason: `-runner none` never starts a
// login run, so the launch, the run's reads, its kill and the attach are
// stubbed. What is real is the console's own mount.

const RUN_PREFIX = "3f1b7c26-0000-4000-8000-0000000a";

interface Sandbox {
  launches: () => number;
  kills: string[];
}

/** Stub whichever launch door `launchPattern` names, the login run it
 *  answers, its kill and its attach. */
async function stubSignInSandbox(page: Page, launchPattern: string): Promise<Sandbox> {
  let launches = 0;
  await page.route(launchPattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    launches++;
    const id = `${RUN_PREFIX}${String(launches).padStart(4, "0")}`;
    await stubAttachTicket(page, id);
    await route.fulfill({ json: { run_id: id, state: "PENDING" } });
  });
  await page.route(new RegExp(`/api/v1/runs/${RUN_PREFIX}\\d{4}$`), async (route) => {
    const id = new URL(route.request().url()).pathname.split("/").pop();
    await route.fulfill({ json: { id, task: "harness login", interactive: true, state: "RUNNING" } });
  });
  const kills: string[] = [];
  await page.route(new RegExp(`/api/v1/runs/${RUN_PREFIX}\\d{4}/kill$`), async (route) => {
    kills.push(new URL(route.request().url()).pathname.split("/").at(-2) ?? "");
    await route.fulfill({ status: 202, json: {} });
  });
  await stubAttachSocket(page, (_n, ws) => ws.send(attachModeFrame(false)));
  return { launches: () => launches, kills };
}

/** Serve /setup/status spliced by `splice`, fetched once and cached (the shell
 *  polls it, and a real round trip per match races Playwright disposing an
 *  in-flight response — model-access-banner.spec.ts's own reason). */
async function spliceStatus(page: Page, splice: (body: Record<string, unknown>) => void): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route: Route) => {
    if (!cached) {
      cached = (await (await route.fetch()).json()) as Record<string, unknown>;
      splice(cached);
    }
    await route.fulfill({ json: cached });
  });
}

const perUserRow = (body: Record<string, unknown>) => {
  body.model_access = { state: "not_configured", mechanism: "bedrock_sso", action: AGENTS.SIGN_IN_AWS };
  body.harnesses = ((body.harnesses ?? []) as { id: string }[]).map((h) =>
    h.id === "claude-code" ? { ...h, enabled: true, mechanism: "bedrock_sso", credential_source: "per_user" } : h,
  );
};

/** Exactly one door, in the dialog layer — none inside the page itself. */
async function expectOneDoor(page: Page, title: string): Promise<void> {
  await expect(page.getByRole("dialog")).toHaveCount(1);
  await expect(page.getByRole("dialog", { name: title })).toBeVisible();
  await expect(page.getByTestId("harness-login-pane")).toHaveCount(1);
  await expect(page.locator("#main-content [data-testid='harness-login-pane']")).toHaveCount(0);
}

test.describe("one door — today's door, where the install has no model providers", () => {
  test("Settings and the Agents tab open the shell's one door, and leaving the route keeps it", async ({ page }) => {
    await spliceStatus(page, perUserRow);
    // A saved per_user roster row, so the Agents tab offers its own sign-in.
    await page.route("**/api/v1/agent-providers", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      await route.fulfill({
        headers: { ETag: '"one-door"' },
        json: {
          agents: [
            {
              id: "claude-code",
              mechanism: "bedrock_sso",
              credential_source: "per_user",
              sso_start_url: "https://acme.awsapps.com/start",
            },
          ],
        },
      });
    });
    const sandbox = await stubSignInSandbox(page, "**/api/v1/setup/harness-login");

    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await page.locator("#lane-bedrock").click();
    await page.getByRole("button", { name: "Sign in with SSO" }).click();
    await expectOneDoor(page, MODEL_ACCESS_BANNER.DIALOG_TITLE);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toHaveCount(0);

    await page.getByTestId("providers-card").getByText(PROVIDERS.CARD_OPEN).click();
    await expect(page).toHaveURL(/\/admin\/providers$/);
    await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
    await page.getByTestId("agent-row-claude-code").getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
    await expectOneDoor(page, MODEL_ACCESS_BANNER.DIALOG_TITLE);
    await page.getByRole("button", { name: /start login/i }).click();
    await expect.poll(sandbox.launches).toBe(1);

    // The single-mount property: the door belongs to the shell, so a route
    // change leaves it open on the same sign-in — no second pane, no second
    // sandbox, nothing killed.
    await navToRoute(page, "/admin/settings");
    await expect(page).toHaveURL(/\/admin\/settings$/);
    await expectOneDoor(page, MODEL_ACCESS_BANNER.DIALOG_TITLE);
    expect(sandbox.launches()).toBe(1);
    expect(sandbox.kills).toEqual([]);
  });
});

test.describe("one door — keyed by provider (User view)", () => {
  const BEDROCK = {
    id: "bedrock-prod",
    name: "Bedrock (prod)",
    kind: "bedrock_sso",
    harnesses: ["claude-code"],
    default_for: ["claude-code"],
    host: "bedrock-runtime.us-east-1.amazonaws.com",
  };

  // #541: Your model connections (Your account) replaces Getting Started's own
  // button as the page every person opens a provider door from — Getting
  // Started keeps only the summary chip and a link there.
  test("Your model connections and the strip open the same provider door, across a route change", async ({
    page,
  }) => {
    await mockMemberRole(page);
    await spliceStatus(page, (body) => {
      perUserRow(body);
      body.model_providers = [BEDROCK];
      body.provider_access = [{ provider: BEDROCK.id, state: "not_configured" }];
    });
    const legacy = await stubSignInSandbox(page, "**/api/v1/setup/harness-login");
    const provider = await stubSignInSandbox(page, `**/api/v1/model-providers/${BEDROCK.id}/sign-in`);

    await gotoConsole(page);
    await navToRoute(page, "/account");
    // The card claims the door, so the strip's own line for this same
    // provider is suppressed — one "Sign in to AWS" on the page, not two.
    await page.getByTestId("model-connections-card").getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true }).click();
    const door = page.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    await expect(door).toContainText("For Bedrock (prod)");
    await expect(door).toContainText(MODEL_ACCESS_BANNER.DIALOG_CLEANUP_NOTE);
    // Packet E draws no consent step: the provider's own sign-in starts at once.
    await expect.poll(provider.launches).toBe(1);
    expect(legacy.launches()).toBe(0);

    await navToRoute(page, "/runs");
    await expect(page).toHaveURL(/\/runs$/);
    await expectOneDoor(page, MODEL_ACCESS_BANNER.DIALOG_TITLE);
    expect(provider.launches()).toBe(1);

    // Cancelled: the sandbox is stopped, and the strip is back with its line.
    await door.getByRole("button", { name: /cancel/i }).first().click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect.poll(() => provider.kills.length).toBeGreaterThanOrEqual(1);
    await expect(page.getByText(BANNER.B1("Claude Code", "Bedrock (prod)"))).toBeVisible();
    await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true }).click();
    await expect(page.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toContainText("For Bedrock (prod)");
    await expect.poll(provider.launches).toBe(2);
    expect(legacy.launches()).toBe(0);
  });

  test("the strip's token line opens the token door, which saves to the provider", async ({ page }) => {
    const GATEWAY = {
      id: "corp-gateway",
      name: "Corp gateway",
      kind: "custom_endpoint",
      harnesses: ["claude-code"],
      default_for: ["claude-code"],
      host: "gateway.corp.example",
    };
    await mockMemberRole(page);
    await spliceStatus(page, (body) => {
      body.model_providers = [GATEWAY];
      body.provider_access = [{ provider: GATEWAY.id, state: "not_configured" }];
    });
    const writes: string[] = [];
    await page.route(`**/api/v1/model-providers/${GATEWAY.id}/credential`, async (route) => {
      writes.push(route.request().postData() ?? "");
      await route.fulfill({ status: 204, body: "" });
    });

    await gotoConsole(page);
    await navToRoute(page, "/runs");
    await expect(page.getByText(BANNER.B4("Claude Code", "Corp gateway", true))).toBeVisible();
    await page.getByRole("button", { name: "Add your token" }).click();
    const door = page.getByRole("dialog", { name: "Add your token for Corp gateway" });
    await expect(door).toContainText("Sent to gateway.corp.example");
    await expect(door).toContainText(KEY_DOOR.NOTE);
    await door.getByLabel("Token").fill("gw-e2e-token");
    await door.getByRole("button", { name: KEY_DOOR.SAVE }).click();
    await expect(page.getByText("Token saved")).toBeVisible();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    expect(writes).toEqual([JSON.stringify({ value: "gw-e2e-token" })]);
  });

  test("Settings' Claude sign-in stays today's door in the Admin view", async ({ page }) => {
    await spliceStatus(page, (body) => {
      body.auth = { ...(body.auth as object), shared_subscription_allowed: true };
      body.providers = ((body.providers ?? []) as { tool?: string }[]).map((p) =>
        p.tool === "claude" ? { ...p, logged_in: false } : p,
      );
      body.harness = ((body.harness ?? []) as { provider?: string }[]).filter((h) => h.provider !== "anthropic");
      body.model_providers = [{ ...BEDROCK, id: "claude-sub", name: "Claude subscription", kind: "anthropic_subscription" }];
      body.provider_access = [{ provider: "claude-sub", state: "not_configured" }];
    });
    const legacy = await stubSignInSandbox(page, "**/api/v1/setup/harness-login");
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await page.locator("#lane-subscription").click();
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    const door = page.getByRole("dialog", { name: CLAUDE_DOOR.TITLE });
    await expect(door).toBeVisible();
    // No provider line: packet E mounts the provider doors in the User view only.
    await expect(door).not.toContainText("For Claude subscription");
    await door.getByRole("button", { name: /start login/i }).click();
    await expect.poll(legacy.launches).toBe(1);
  });
});
