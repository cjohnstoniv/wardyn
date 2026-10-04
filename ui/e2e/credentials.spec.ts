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

  // Key custody (M5 S2/S3): a security admin sees the declared domains (the e2e backend declares
  // none, so only default), assigns everyone to a domain, sees the Key domain column say so, and
  // removes the assignment. A refused write keeps its dialog open with the server's sentence.
  test("a security admin manages key-domain assignments and sees each person's key domain", async ({ page }) => {
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
      data: { value: "e2e-key-domain-token-0123456789" },
    });
    expect(put.status(), await put.text()).toBe(204);

    await asRealSecurityAdmin(page);
    await openCredentials(page);
    const card = page.locator("section").filter({ has: page.getByRole("heading", { name: "Key domains", exact: true }) });
    await expect(card).toBeVisible();
    await expect(card.getByRole("row").filter({ hasText: "default" }).first()).toContainText("Proven");

    // Nothing assigned: the person's next key is in default.
    const personRow = page.getByRole("row").filter({ hasText: person.email });
    await expect(personRow).toContainText("default · default");

    // A domain the file does not declare is not offered; assign everyone to default.
    await card.getByRole("button", { name: "Assign a domain" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByLabel("Domain").locator("option")).toHaveText(["default"]);
    await dialog.getByRole("radio", { name: "Everyone" }).check();
    await dialog.getByRole("button", { name: "Assign a domain" }).click();
    await expect(dialog).toHaveCount(0);
    const assignmentRow = card.getByRole("row").filter({ hasText: "everyone" });
    await expect(assignmentRow).toContainText("default");
    await expect(card.getByText("Submitted for approval")).toHaveCount(0);

    // The column now says why.
    await page.reload();
    await expect(page.getByRole("row").filter({ hasText: person.email })).toContainText("default · everyone");

    await card.getByRole("row").filter({ hasText: "everyone" }).getByRole("button", { name: "Remove" }).click();
    const confirm = page.getByRole("alertdialog");
    await expect(confirm).toContainText('Remove the domain assignment for "everyone"?');
    await confirm.getByRole("button", { name: "Remove" }).click();
    await expect(card.getByRole("row").filter({ hasText: "everyone" })).toHaveCount(0);
    await page.reload();
    await expect(page.getByRole("row").filter({ hasText: person.email })).toContainText("default · default");
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

// #1477: no one can create a token that acts as another person (the mint route
// is refused), so the console lists the admin-created tokens that already exist
// — metadata only — and revokes them. Real backend: legacy rows are seeded the
// way an older release left them (a minter that is not the owner), and the list
// and the revoke are the daemon's own GET /tokens?minted_for_others=true and
// DELETE /tokens/{id}.
test.describe("Tokens an admin created for someone else (Admin view, #1477)", () => {
  function seedMinted(name: string, minter: string): { email: string; id: string } {
    const sfx = randomBytes(4).toString("hex");
    const email = `minted-${sfx}@e2e.wardyn.invalid`;
    const id = sql(
      `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at, minted_by)
       VALUES (gen_random_uuid(), 'e2e-minted-${sfx}', '${email}', 'user', 'standard', '[]'::jsonb, false, '${name}', '${createHash("sha256").update(sfx).digest("hex")}', now(), '${minter}')
       RETURNING id`,
    )
      .split("\n")[0]
      .trim();
    return { email, id };
  }

  test("nothing to list: the empty line and the rule", async ({ page }) => {
    await openCredentials(page);
    const section = page.getByRole("region", { name: "Tokens an admin created for someone else" });
    await expect(section.getByText("No admin has created a token for someone else.")).toBeVisible();
    await expect(
      section.getByText("No one can create a token that acts as another person. They sign in and create their own."),
    ).toBeVisible();
    await expect(section.getByRole("table")).toHaveCount(0);
  });

  test("lists a legacy token without its value, and Revoke asks first, then ends it", async ({ page }) => {
    const t = seedMinted("ci-e2e", "e2e-admin@e2e.wardyn.invalid");
    await openCredentials(page);
    const section = page.getByRole("region", { name: "Tokens an admin created for someone else" });
    const row = section.getByRole("row").filter({ hasText: t.email });
    await expect(row).toBeVisible();
    await expect(row).toContainText("ci-e2e");
    await expect(row).toContainText("e2e-admin@e2e.wardyn.invalid");
    await expect(section.getByText("1 still works")).toBeVisible();
    await expect(page.locator("body")).not.toContainText("wdn_");

    await row.getByRole("button", { name: "Revoke" }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(`Revoke ci-e2e for ${t.email}?`)).toBeVisible();
    await expect(dialog.getByText(`It stops working now. ${t.email} can create their own after signing in.`)).toBeVisible();
    expect(sql(`SELECT revoked_at IS NULL FROM api_tokens WHERE id = '${t.id}'`)).toBe("t");
    await dialog.getByRole("button", { name: "Revoke token" }).click();
    await expect(page.getByText("Token revoked.")).toBeVisible();
    await expect(section.getByText("No admin has created a token for someone else.")).toBeVisible();
    expect(sql(`SELECT revoked_at IS NULL FROM api_tokens WHERE id = '${t.id}'`)).toBe("f");
  });

  test("a self-minted token is never listed as admin-created", async ({ page }) => {
    const sfx = randomBytes(4).toString("hex");
    const email = `selfmint-${sfx}@e2e.wardyn.invalid`;
    sql(
      `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
       VALUES (gen_random_uuid(), 'e2e-self-${sfx}', '${email}', 'user', 'standard', '[]'::jsonb, false, 'mine', '${createHash("sha256").update(`self${sfx}`).digest("hex")}', now())`,
    );
    await openCredentials(page);
    const section = page.getByRole("region", { name: "Tokens an admin created for someone else" });
    await expect(section.getByText("No admin has created a token for someone else.")).toBeVisible();
    await expect(section.getByText(email)).toHaveCount(0);
  });
});

