/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542 (design §5.6, packet MP-C) — the New Run rail's provider picker, live
// in a browser: picking a provider actually reaches the launch request, and
// Launch actually navigates.
//
// Same harness ceiling as model-access-banner.spec.ts's mockModelAccess: the
// seeded e2e backend authenticates every spec with a bare admin bearer token,
// which credentialOwner (model_provider_credentials.go) refuses as "no
// person" — so this harness can never hold a REAL per-person model-provider
// credential, and a genuine launch through a configured provider would 422 on
// the liveness check (providerLiveness) no matter which provider kind is
// picked. GET /setup/status is spliced with a provider block exactly as that
// spec splices model_access onto the same real response, and the launch POST
// is stubbed to a clean 201 — the same route-stub boundary
// model-providers.spec.ts and model-provider-editor.spec.ts already draw for
// the admin config screens, moved to the rail's own wire contract.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { RAIL_PROVIDER } from "../src/app/components/wardyn/copy";

const BEDROCK = {
  id: "bedrock-prod",
  name: "Bedrock (prod)",
  kind: "bedrock_sso",
  harnesses: ["claude-code"],
  host: "bedrock-runtime.us-east-1.amazonaws.com",
};
const GATEWAY = {
  id: "corp-gateway",
  name: "Corp gateway",
  kind: "custom_endpoint",
  harnesses: ["claude-code", "codex-cli"],
  host: "gateway.corp.example",
};

/** Splices a provider block onto /setup/status — cached and served, never
 *  re-fetched per match (mockModelAccess's own reasoning, model-access-banner.spec.ts):
 *  the landing redirect, the shell's poll and the screen's mount all hit this
 *  endpoint, and a real round trip per match races Playwright disposing an
 *  in-flight response. */
async function mockProviders(
  page: Page,
  rows: { provider: typeof BEDROCK | typeof GATEWAY; state: string; defaultFor?: string[] }[],
): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.model_providers = rows.map((r) => ({ ...r.provider, default_for: r.defaultFor }));
      body.provider_access = rows.map((r) => ({ provider: r.provider.id, state: r.state }));
      cached = body;
    }
    await route.fulfill({ json: cached! });
  });
}

test.describe("New Run rail — the provider picker (#542)", () => {
  test("R2/R6: several candidates, no admin default — Launch waits, then reaches the wire on a pick", async ({ page }) => {
    await mockProviders(page, [
      { provider: BEDROCK, state: "live" },
      { provider: GATEWAY, state: "live" },
    ]);
    let sent: Record<string, unknown> | null = null;
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      sent = route.request().postDataJSON() as Record<string, unknown>;
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: "e2e-provider-run" }) });
    });

    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await page.getByLabel("Title").fill("Pick a provider e2e");

    // R6 (QC-4): neither candidate is the admin default, so nothing is
    // preselected and Launch waits for an explicit choice.
    await expect(page.getByText(RAIL_PROVIDER.LAUNCH_HINT)).toBeVisible();
    await expect(page.getByRole("button", { name: "Launch run" })).toBeDisabled();

    await page.getByRole("combobox", { name: RAIL_PROVIDER.LABEL }).click();
    await page.getByRole("option", { name: RAIL_PROVIDER.OPTION("Corp gateway", "token", "added") }).click();

    await expect(page.getByRole("button", { name: "Launch run" })).toBeEnabled();
    await page.getByRole("button", { name: "Launch run" }).click();

    await expect(page).toHaveURL(/\/runs\/e2e-provider-run/);
    expect(sent?.model_provider).toBe(GATEWAY.id);
  });

  test("R1: a sole candidate needs no picker, and its id still reaches the wire", async ({ page }) => {
    await mockProviders(page, [{ provider: BEDROCK, defaultFor: ["claude-code"], state: "live" }]);
    let sent: Record<string, unknown> | null = null;
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      sent = route.request().postDataJSON() as Record<string, unknown>;
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: "e2e-provider-run-solo" }) });
    });

    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByLabel("Title").fill("Sole provider e2e");

    await expect(page.getByText(RAIL_PROVIDER.STATIC("Bedrock (prod)"))).toBeVisible();
    await expect(page.getByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toHaveCount(0);

    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page).toHaveURL(/\/runs\/e2e-provider-run-solo/);
    expect(sent?.model_provider).toBe(BEDROCK.id);
  });
});
