/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Why can't I?" card (board M2, Option A) on three refusal kinds: a refused
// launch, a refused attach and a refused egress row on the run's Audit tab. Each
// shows which wall set the limit, who owns it and how to ask. The policy reference
// is spliced at the wire (the seeded daemon has no governance profile with a
// contact); the strings are the board's, written out so a copy edit fails here.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, mockPreflightClear, navTo, navToRoute, newRunHeading, submitLaunch, walkGuidedToReview } from "./fixtures";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { findRunningFixture } from "./attach-stub";
import { ATTACH_CAP_REASON } from "../src/app/lib/run-entry";

const POLICY = {
  source: "profile",
  layer: "profile",
  name: "Platform team",
  owner: "Platform Security",
  request_url: "https://example.com/access",
};
// What GET /runs/{id} carries as the run's own policy: no layer, since only a refusal's own reference names one.
const RUN_POLICY = { source: "profile", name: "Platform team", owner: "Platform Security", request_url: "https://example.com/access" };

async function asMember(page: Page): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.principal = "bob@acme.example";
    json.role = "user";
    json.operator = false;
    json.security_operator = false;
    await route.fulfill({ response, json });
  });
}

test("a refused launch shows the card under the server's sentence", async ({ page }) => {
  const sentence = "Your governance profile allows 2 runs at once.";
  await asMember(page);
  await page.route("**/api/v1/runs", async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    await route.fulfill({ status: 422, json: { error: sentence, reason: "run_quota", policy: POLICY } });
  });
  await mockPreflightClear(page);
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await expect(newRunHeading(page, NR_START.TITLE)).toBeVisible();
  await walkGuidedToReview(page);
  const naming = await submitLaunch(page, "e2e why card launch");

  await expect(naming.getByRole("alert")).toContainText(sentence);
  const card = naming.getByTestId("policy-remedy");
  await expect(card).toContainText("Set byPlatform team walls");
  await expect(card).toContainText("OwnerPlatform Security");
  await expect(card).not.toContainText("Can change");
  await expect(card.getByRole("link", { name: "Request access" })).toHaveAttribute("href", "https://example.com/access");
});

test("a refused attach shows the card with where it was enforced and Copy details", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  // Cache the first read, as stubInteractiveRun does: the run page polls it and a disposed response fails the case.
  let cached: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/runs/${runId}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    if (!cached) {
      const response = await route.fetch();
      cached = (await response.json()) as Record<string, unknown>;
      cached.interactive = true;
      cached.state = "RUNNING";
      cached.policy = RUN_POLICY;
    }
    await route.fulfill({ json: cached });
  });
  await page.route(`**/api/v1/runs/${runId}/attach/ticket`, (route) =>
    route.fulfill({
      // The real cap (attach_ticket.go): a 503 naming its reason, with no policy.
      status: 503,
      json: { error: "too many open terminal sessions for this run (max 4) — close one and retry", reason: ATTACH_CAP_REASON },
    }),
  );
  await page.goto(`/runs/${runId}`);

  const card = page.getByTestId("run-terminal-pane").getByTestId("policy-remedy");
  // No policy in the envelope, so the card is the run's own: Owner and the ask, no wall named.
  await expect(card).not.toContainText("Set by");
  await expect(card).toContainText("OwnerPlatform Security");
  await expect(card).toContainText("EnforcedBy Wardyn, before the terminal opens");
  await expect(card.getByRole("link", { name: "Request access" })).toBeVisible();
  await expect(card.getByRole("button", { name: "Copy details" })).toBeVisible();
});

test("a refused egress row on the Audit tab shows the card on its own line", async ({ page }) => {
  await asMember(page);
  await page.route("**/api/v1/runs/*", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    if (json.task === "e2e fixture 6") json.policy = RUN_POLICY;
    await route.fulfill({ response, json });
  });
  await page.route("**/api/v1/audit*", async (route) => {
    const response = await route.fetch();
    const rows = (await response.json()) as Record<string, unknown>[];
    if (new URL(route.request().url()).searchParams.has("action") || !Array.isArray(rows)) {
      return route.fulfill({ response, json: rows });
    }
    rows.unshift({
      id: randomUUID(),
      time: new Date().toISOString(),
      run_id: rows[0]?.run_id,
      actor_type: "system",
      actor: "proxy",
      action: "egress.deny",
      target: "pypi.org",
      outcome: "deny",
      data: { rule_source: "policy:denied" },
    });
    await route.fulfill({ response, json: rows });
  });
  await gotoConsole(page);
  await navTo(page, "Runs");
  await page.getByText("e2e fixture 6").click();
  await page.getByRole("tab", { name: /audit/i }).click();

  const row = page.getByTestId("run-audit-row").filter({ hasText: "pypi.org" });
  await expect(row.getByText("Refused by policy")).toBeVisible();
  const card = row.getByTestId("policy-remedy");
  // The run's policy reference carries no layer and the proxy cannot know one, so no wall is named.
  await expect(card).not.toContainText("Set by");
  await expect(card).toContainText("OwnerPlatform Security");
  await expect(card.getByRole("link", { name: "Request access" })).toBeVisible();
});
