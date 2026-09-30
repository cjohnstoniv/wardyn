/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #540 (design §5.5, packet MP-D): the shell strip's PROVIDER-mode states —
// graded by providerAttention() and rendered by ModelAccessBanner's
// providerStripLine/BANNER table — carried no e2e pin before this file.
// model-access-banner.spec.ts pins where the strip rides (every screen, focus
// mode, never Getting Started); refusal-doors.spec.ts's withProviders() grades every provider
// `not_configured` with no provider named anyone's default, which never
// satisfies providerAttention's `missing` arm, so B1/B3/B4/B5/B8 never render
// in either suite. Pinned here, against the real console, with only
// /setup/status spliced (the seeded backend has no people to grade a real
// per-user provider credential for).

import type { Page, Route } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { BANNER, CONNECTIONS, CLAUDE_DOOR, KEY_DOOR } from "../src/app/components/wardyn/copy/door";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";

const P = {
  bedrock: { id: "bedrock-prod", name: "Bedrock (prod)", kind: "bedrock_sso" },
  gateway: { id: "corp-gateway", name: "Corp gateway", kind: "custom_endpoint", host: "gateway.corp.example" },
  claude: { id: "claude-sub", name: "Claude subscription", kind: "anthropic_subscription" },
};

/** Splices the provider block onto /setup/status, cached and served — the same
 *  technique refusal-doors.spec.ts's withProviders() and one-door.spec.ts use:
 *  the landing redirect, the shell's own poll and a screen's mount all hit
 *  this endpoint, and a real round trip per match races Playwright disposing
 *  an in-flight response. `defaults` names which provider id is the default
 *  for which harnesses — the one field refusal-doors.spec.ts's own fixture
 *  never sets, and the one providerAttention's `missing` arm requires. */
async function withProviderAttention(
  page: Page,
  providers: (typeof P)[keyof typeof P][],
  access: Record<string, { state: string; deadline?: string }>,
  defaults: Record<string, string[]> = {},
): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route: Route) => {
    if (!cached) {
      cached = (await (await route.fetch()).json()) as Record<string, unknown>;
      cached.model_providers = providers.map((p) => ({
        ...p,
        harnesses: ["claude-code", "codex-cli"],
        default_for: defaults[p.id] ?? [],
      }));
      cached.provider_access = providers.map((p) => ({ provider: p.id, ...access[p.id] }));
    }
    await route.fulfill({ json: cached! });
  });
  // The AWS and Claude doors start their own sign-in at once; nothing here
  // signs in (this file pins the STRIP, not the sign-in round trip — that's
  // signin-door-aws.spec.ts / signin-door-claude.spec.ts).
  await page.route("**/api/v1/model-providers/*/sign-in", (route) =>
    route.fulfill({ status: 503, json: { error: "e2e: no sign-in here" } }),
  );
}

test.describe("the shell strip under a provider block (design §5.5, #540)", () => {
  test("B1: the Bedrock default's not-signed-in line opens the AWS door, and Not now dismisses it for the session", async ({
    page,
  }) => {
    await withProviderAttention(page, [P.bedrock], { [P.bedrock.id]: { state: "not_configured" } }, {
      [P.bedrock.id]: ["claude-code"],
    });
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    const line = BANNER.B1("Claude Code", P.bedrock.name);
    await expect(page.getByText(line)).toBeVisible();
    await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
    await expect(page.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await page.keyboard.press("Escape");

    await page.getByRole("button", { name: "Not now" }).click();
    await expect(page.getByText(line)).toHaveCount(0);
    // …and it stays cleared across a navigation, not just this render (packet
    // MP-D QD-3).
    await navToRoute(page, "/workspaces");
    await expect(page.getByText(line)).toHaveCount(0);
  });

  test("B4: a Codex CLI default with no token names the harness and opens the token door", async ({ page }) => {
    await withProviderAttention(page, [P.gateway], { [P.gateway.id]: { state: "not_configured" } }, {
      [P.gateway.id]: ["codex-cli"],
    });
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    await expect(page.getByText(BANNER.B4("Codex CLI", P.gateway.name, true))).toBeVisible();
    await page.getByRole("button", { name: CONNECTIONS.ADD_TOKEN }).click();
    await expect(page.getByRole("dialog", { name: KEY_DOOR.TITLE(true, P.gateway.name) })).toBeVisible();
  });

  test("B5: a Claude subscription default: the sign-in sentence opens the Claude door", async ({ page }) => {
    await withProviderAttention(page, [P.claude], { [P.claude.id]: { state: "not_configured" } }, {
      [P.claude.id]: ["claude-code"],
    });
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    await expect(page.getByText(BANNER.B5("Claude Code"))).toBeVisible();
    await page.getByRole("button", { name: CONNECTIONS.SIGN_IN_CLAUDE }).click();
    await expect(page.getByRole("dialog", { name: CLAUDE_DOOR.TITLE })).toBeVisible();
  });

  test("B8: two providers needing attention collapse to a count, and Review goes to Getting started", async ({ page }) => {
    await withProviderAttention(
      page,
      [P.bedrock, P.gateway],
      { [P.bedrock.id]: { state: "not_configured" }, [P.gateway.id]: { state: "not_configured" } },
      { [P.bedrock.id]: ["claude-code"], [P.gateway.id]: ["codex-cli"] },
    );
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    await expect(page.getByText(BANNER.B8(2))).toBeVisible();
    // Neither single-provider line renders while there are two (packet MP-D
    // QD-2: two or more COLLAPSE, they do not stack).
    await expect(page.getByText(BANNER.B1("Claude Code", P.bedrock.name))).toHaveCount(0);
    await expect(page.getByText(BANNER.B4("Codex CLI", P.gateway.name, true))).toHaveCount(0);
    await page.getByRole("link", { name: BANNER.REVIEW }).click();
    await expect(page).toHaveURL(/\/account$/);
  });

  test("an expired Bedrock default's provider-strip line is User-view only", async ({ page }) => {
    await withProviderAttention(page, [P.bedrock], { [P.bedrock.id]: { state: "expired_signin" } }, {
      [P.bedrock.id]: ["claude-code"],
    });
    await gotoConsole(page);
    await navToRoute(page, "/runs");
    const line = CONNECTIONS.C6_LINE(P.bedrock.name);
    await expect(page.getByText(line)).toBeVisible();

    // providerMode's strip is gated on userView independently of the legacy
    // path's adminViewSuppressed (model-access-banner.tsx's `lines` variable) —
    // the one gate this file pins that model-access-banner.spec.ts's own
    // admin/user-view test (the legacy `door.needsAttention` path) does not.
    await navToRoute(page, "/admin/runs");
    await expect(page.getByText(line)).toHaveCount(0);
  });
});
