/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// E2E coverage for the admin "Stored credentials" page (design F-1/F-2,
// packet F §4) and its erase flow (F-5/F-6, packet F §5) —
// src/app/components/screens/credentials.tsx.
//
// Real backend, no stubbed routes: a genuine model-provider config (PUT
// /model-providers as the admin bearer), a genuine per-person credential (PUT
// /model-providers/{id}/credential as that person's own api_tokens row — the
// #698 pattern model-provider-cases.spec.ts already uses), then the actual
// security_admin session (asRealSecurityAdmin) reading GET
// /model-providers/credentials and erasing with DELETE
// /people/{principal}/credentials — CS-5/CS-6, already shipped; this spec pins
// the console built on top of them.
import { createHash, randomBytes } from "node:crypto";
import type { APIRequestContext, Page } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, asRealMember, asRealSecurityAdmin, gotoConsole, navTo, sql } from "./fixtures";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

async function putModelProviders(ctx: APIRequestContext, providers: unknown[]): Promise<void> {
  const cur = await ctx.get("/api/v1/model-providers", { headers: auth });
  const etag = cur.headers()["etag"] ?? null;
  const headers: Record<string, string> = { ...auth };
  if (etag) headers["If-Match"] = etag;
  const res = await ctx.put("/api/v1/model-providers", { headers, data: { providers } });
  expect(res.status(), await res.text()).toBe(200);
}

// A genuine per-person caller with a DISTINCT subject and email — the same
// api_tokens shape e2e-backend.sh seeds e2e-member/e2e-security-admin with —
// so the inventory row's person (subject) and email (design F-2) are two
// different strings a reader could tell apart, not one string doing both jobs.
function seedPerson(): { auth: { Authorization: string }; subject: string; email: string } {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const subject = `e2e-cred-subject-${hash.slice(0, 8)}`;
  const email = `cred-owner-${hash.slice(0, 8)}@e2e.wardyn.invalid`;
  sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
     VALUES (gen_random_uuid(), '${subject}', '${email}', 'user', 'standard', '[]'::jsonb, false, 'e2e', '${hash}', now())`,
  );
  return { auth: { Authorization: `Bearer ${raw}` }, subject, email };
}

async function openCredentials(page: Page): Promise<void> {
  await gotoConsole(page, "admin");
  await navTo(page, "Credentials");
  await expect(page.getByRole("heading", { name: "Stored credentials", exact: true })).toBeVisible();
}

test.describe.configure({ mode: "serial" });

test.describe("Stored credentials (Admin view)", () => {
  test.beforeEach(async ({ page }) => {
    // Reset the model-providers block so this spec's fixture is the only
    // provider on it, whatever an earlier spec file left behind.
    await putModelProviders(page.request, []);
  });

  test.afterAll(async ({ request }) => {
    await putModelProviders(request, []);
  });

  test("a security admin sees the row's email and provider name, and erases it with the typed confirm", async ({
    page,
  }) => {
    await putModelProviders(page.request, [
      {
        id: "corp-gateway",
        name: "Corp gateway",
        kind: "custom_endpoint",
        base_url: "https://gateway.corp.example",
        harnesses: [{ harness: "claude-code", path: "/anthropic" }],
      },
    ]);
    const person = seedPerson();
    const put = await page.request.put("/api/v1/model-providers/corp-gateway/credential", {
      headers: person.auth,
      data: { value: "e2e-cred-owner-token-0123456789" },
    });
    expect(put.status(), await put.text()).toBe(204);

    // K5-A: a security admin — not just a super admin — reads this page.
    await asRealSecurityAdmin(page);
    await openCredentials(page);

    const row = page.getByRole("row").filter({ hasText: person.email });
    await expect(row).toBeVisible();
    await expect(row).toHaveText(new RegExp(person.subject));
    await expect(row).toHaveText(/Corp gateway/);
    await expect(row).toHaveText(/Stored/);

    // The value itself never appears anywhere on the page.
    await expect(page.locator("body")).not.toContainText("e2e-cred-owner-token-0123456789");

    await row.getByRole("button", { name: "Erase credentials" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(`Erase every credential ${person.email} stored?`)).toBeVisible();

    const confirmBtn = dialog.getByRole("button", { name: "Erase credentials" });
    await expect(confirmBtn).toBeDisabled();

    // Case-insensitive typed-email confirm (design F-5).
    const field = dialog.getByLabel(`Type ${person.email} to confirm`);
    await field.fill(person.email.toUpperCase());
    await expect(confirmBtn).toBeEnabled();
    await confirmBtn.click();

    await expect(dialog.getByText(`Erased 1 credential for ${person.email}.`)).toBeVisible();
    await expect(dialog.getByText("Recorded in the Audit log as credential.erase.")).toBeVisible();
    await dialog.getByRole("button", { name: "Close" }).first().click();

    // The erased person's row is gone once the inventory reloads.
    await expect(page.getByRole("row").filter({ hasText: person.email })).toHaveCount(0);

    // The erase actually landed server-side: the person now has no credential.
    const relist = await page.request.get("/api/v1/model-providers/credentials", { headers: auth });
    expect(relist.ok()).toBeTruthy();
    const body = (await relist.json()) as { credentials: { person: string }[] };
    expect(body.credentials.some((c) => c.person === person.subject)).toBe(false);
  });

  test("erasing by email reaches someone not on the list, and refuses an unknown address", async ({ page }) => {
    await putModelProviders(page.request, [
      {
        id: "corp-gateway",
        name: "Corp gateway",
        kind: "custom_endpoint",
        base_url: "https://gateway.corp.example",
        harnesses: [{ harness: "claude-code", path: "/anthropic" }],
      },
    ]);
    const person = seedPerson();
    // This person holds a SECRET, not a model-provider credential, so they
    // never appear in the inventory rows — exactly the case F-5 built
    // "Erase someone's credentials" for.
    const put = await page.request.put(`/api/v1/secrets/e2e-cred-secret`, {
      headers: { ...person.auth, "Content-Type": "application/json" },
      data: { value: "e2e-cred-secret-value-0123456789" },
    });
    expect(put.status(), await put.text()).toBe(204);

    await asRealSecurityAdmin(page);
    await openCredentials(page);
    await expect(page.getByRole("row").filter({ hasText: person.email })).toHaveCount(0);

    await page.getByRole("button", { name: "Erase someone's credentials" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel("Email or subject").fill(person.email);
    await dialog.getByRole("button", { name: "Erase credentials" }).click();
    await expect(dialog.getByText(`Erased 1 credential for ${person.email}.`)).toBeVisible();
    await dialog.getByRole("button", { name: "Close" }).first().click();

    // An address nobody signs in as is refused with the server's own
    // sentence, and the dialog stays open to retry.
    await page.getByRole("button", { name: "Erase someone's credentials" }).click();
    const retryDialog = page.getByRole("dialog");
    await retryDialog.getByLabel("Email or subject").fill("nobody-at-all@e2e.wardyn.invalid");
    await retryDialog.getByRole("button", { name: "Erase credentials" }).click();
    await expect(retryDialog.getByText(/doesn't match anyone this deployment knows/i)).toBeVisible();
    await expect(retryDialog).toBeVisible();
  });

  test("a member is refused at the route", async ({ page }) => {
    await asRealMember(page);
    await page.goto("/admin/credentials");
    await expect(page.getByText(/This page is part of the admin view/i)).toBeVisible();
  });
});
