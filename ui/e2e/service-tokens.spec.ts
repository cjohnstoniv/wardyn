/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// E2E coverage for #1508's console half (mock M-SVC v2, approved 2026-10-12):
// src/app/components/screens/settings/{invitations-card,service-identities-card,token-dialog}.tsx and
// src/app/components/screens/credentials-minted-tokens.tsx.
//
// The State column and its toggle run against the real backend: admin-minted tokens, a service identity and
// a move row are seeded in Postgres, and the security admin reads them through GET /tokens. The two Account
// cards need a signed-in browser session (a claim or a mint is refused to a bearer token), which this
// backend does not have, so their routes are answered here and the spec proves what the console does with
// the answer: the token is shown once and is in the page nowhere after the dialog closes.
import { createHash, randomBytes } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, asRealSecurityAdmin, expandCard, gotoConsole, navTo, navToRoute, sql } from "./fixtures";

const SECRET = "wdn_e2eonly0001_Qm3xV7pLzK9aRt2WbN8yHc5dJf4sUe6gEXAMPLE";

function seedAdminMinted(email: string, opts: { revoked?: boolean; service?: string } = {}): string {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const id = sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at, minted_by, revoked_at)
     VALUES (gen_random_uuid(), 'sub-${hash.slice(0, 8)}', '${email}', 'user', 'standard', '[]'::jsonb, false, 'job-${hash.slice(0, 6)}', '${hash}', now() - interval '3 days', 'e2e-ops@e2e.wardyn.invalid', ${opts.revoked ? "now() - interval '1 day'" : "NULL"})
     RETURNING id`,
  )
    .split("\n")[0]
    .trim();
  if (opts.service) {
    sql(`INSERT INTO api_token_moves (token_id, service_identity, linked_by) VALUES ('${id}', '${opts.service}', 'e2e-ops')`);
  }
  return id;
}

test.describe("Admin > Credentials: the State column (#1508)", () => {
  test("each admin-minted token says where it stands, and the toggle adds the revoked ones", async ({ page }) => {
    const tag = randomBytes(4).toString("hex");
    const service = `e2e-svc-${tag}`;
    sql(`INSERT INTO service_identities (name, sponsor, created_by) VALUES ('${service}', 'e2e-sponsor', 'e2e-ops')`);
    seedAdminMinted(`needs-${tag}@e2e.wardyn.invalid`);
    seedAdminMinted(`linked-${tag}@e2e.wardyn.invalid`, { service });
    seedAdminMinted(`gone-${tag}@e2e.wardyn.invalid`, { revoked: true });

    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Credentials");
    const section = page.locator("section").filter({ has: page.getByRole("heading", { name: "Tokens an admin created for someone else" }) });
    const rowOf = (who: string) => section.getByRole("row").filter({ hasText: `${who}-${tag}@e2e.wardyn.invalid` });

    await expect(section.getByRole("columnheader", { name: "State" })).toBeVisible();
    await expect(rowOf("needs")).toContainText("Needs a decision");
    await expect(rowOf("needs")).toContainText("Still works as the person. Invite them, or move it to a service identity.");
    await expect(rowOf("linked")).toContainText("Linked to a service identity");
    await expect(rowOf("linked")).toContainText(`Service identity: ${service}`);
    await expect(rowOf("gone")).toHaveCount(0);
    await expect(section).toContainText("Invite the owner with wardyn person invite, or move the token with wardyn token move. The console does neither.");

    await section.getByRole("checkbox", { name: "Also show revoked tokens" }).click();
    await expect(rowOf("gone")).toContainText("Revoked");
    await expect(rowOf("gone")).toContainText("with no replacement.");
    await expect(rowOf("gone").getByRole("button", { name: "Revoke" })).toHaveCount(0);
    await expect(rowOf("needs").getByRole("button", { name: "Revoke" })).toBeVisible();
  });
});

async function stubAccountRoutes(page: Page): Promise<void> {
  // The one waiting invitation is gone once it is claimed, as the server would list it.
  let claimed = false;
  await page.route("**/api/v1/me/invitations**", async (route) => {
    const req = route.request();
    if (req.method() === "POST") {
      claimed = true;
      return route.fulfill({
        status: 201,
        json: { id: "tok-claimed", principal: "e2e-member", role: "user", user_type: "standard", name: "ci-laptop", created_at: new Date().toISOString(), expires_at: new Date(Date.now() + 30 * 86_400_000).toISOString(), token: SECRET },
      });
    }
    if (claimed) return route.fulfill({ json: [] });
    return route.fulfill({
      json: [{ id: "6f2a1c90-5b7e-4d21-8a43-c0e9d2b71f05", target: "e2e-member", created_by: "ops-admin@example.com", created_at: new Date().toISOString(), expires_at: new Date(Date.now() + 2 * 86_400_000).toISOString(), token_name: "ci-laptop", state: "pending" }],
    });
  });
  await page.route("**/api/v1/me/service-identities**", async (route) => {
    const req = route.request();
    if (req.url().includes("/tokens")) {
      if (req.method() === "POST") {
        return route.fulfill({
          status: 201,
          json: { id: "tok-minted", principal: "service:ci-nightly", role: "user", user_type: "standard", name: "deploy", created_at: new Date().toISOString(), expires_at: new Date(Date.now() + 90 * 86_400_000).toISOString(), token: SECRET },
        });
      }
      return route.fulfill({ json: [] });
    }
    return route.fulfill({
      json: [{ name: "ci-nightly", description: "Nightly build and test runs", sponsor: "e2e-member", sponsor_since: new Date().toISOString(), user_type: "standard", created_by: "ops", created_at: new Date().toISOString(), state: "active" }],
    });
  });
}

async function expectTokenGone(page: Page): Promise<void> {
  await expect(page.getByTestId("show-once-token")).toHaveCount(0);
  await expect(page.locator("body")).not.toContainText(SECRET);
  expect(page.url()).not.toContain(SECRET);
  const stored = await page.evaluate(() => JSON.stringify([{ ...localStorage }, { ...sessionStorage }]));
  expect(stored).not.toContain(SECRET);
}

test.describe("Your account: the token cards (#1508)", () => {
  test("claiming an invitation shows the token once, and nothing keeps it", async ({ page }) => {
    await asRealMember(page);
    await stubAccountRoutes(page);
    await gotoConsole(page, "user");
    await navToRoute(page, "/account");

    await expandCard(page, "Invitations");
    const card = page.getByTestId("invitations-card");
    await expect(card).toContainText("Invitation from ops-admin@example.com");
    await card.getByRole("button", { name: /^Claim invitation from/ }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("heading", { name: "Claim your token" })).toBeVisible();
    await expect(dialog.getByLabel(/Token name/)).toHaveValue("ci-laptop");
    await dialog.getByRole("button", { name: "Claim token" }).click();

    await expect(dialog.getByRole("heading", { name: "Your new token" })).toBeVisible();
    await expect(dialog.getByTestId("show-once-token")).toHaveText(SECRET);
    await expect(dialog.getByText("you (User)")).toBeVisible();
    // A first Done without a copy only warns; a click outside never closes a token.
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog.getByText("You haven't copied it yet. Once you close this, it can't be shown again.")).toBeVisible();
    await page.mouse.click(5, 5);
    await expect(dialog.getByTestId("show-once-token")).toBeVisible();
    await dialog.getByRole("button", { name: "Close without copying" }).click();
    await expect(dialog).toHaveCount(0);
    await expectTokenGone(page);
    // The last waiting invitation was claimed, so the card is gone and focus lands on the page, not <body> (FUG-L356).
    await expect(card).toHaveCount(0);
    await expect(page.locator("#main-content")).toBeFocused();
  });

  test("minting a service token shows it once, and nothing keeps it", async ({ page }) => {
    await asRealMember(page);
    await stubAccountRoutes(page);
    await gotoConsole(page, "user");
    await navToRoute(page, "/account");

    await expandCard(page, "Service identities I sponsor");
    const card = page.getByTestId("service-identities-card");
    await expect(card).toContainText("ci-nightly");
    await card.getByRole("button", { name: "Mint a token for ci-nightly" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("heading", { name: "Mint a token for ci-nightly" })).toBeVisible();
    await dialog.getByLabel(/Token name/).fill("deploy");
    await dialog.getByRole("button", { name: "Mint token" }).click();

    await expect(dialog.getByTestId("show-once-token")).toHaveText(SECRET);
    await expect(dialog.getByText("service:ci-nightly")).toBeVisible();
    await dialog.getByRole("button", { name: "Done" }).click();
    await dialog.getByRole("button", { name: "Close without copying" }).click();
    await expect(dialog).toHaveCount(0);
    await expectTokenGone(page);
    await expect(card.getByRole("button", { name: "Mint a token for ci-nightly" })).toBeFocused();
  });
});
