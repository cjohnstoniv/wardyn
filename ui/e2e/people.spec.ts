/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { createHash, randomBytes } from "node:crypto";
import { test, expect, ADMIN_TOKEN, asRealMember, asRealSecurityAdmin, gotoConsole, navTo, sql } from "./fixtures";

// 0.8.6 ppl-p2 (mock M12): the admin People page. A person is pre-created through POST /people and holds
// an SSH key, so the page lists them from GET /people with real counts. The e2e backend has no sign-in
// provider, so POST /sessions/revoke is not mounted on it: that one door is answered by a stub, and the
// spec asserts the request the page sends. SSH key removal is real, down to its audit row.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
// The key fixture internal/api/sshkeys_test.go and ssh-keys.spec.ts already use.
const PUBLIC_KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT alice@laptop";

async function seedPerson(request: import("@playwright/test").APIRequestContext) {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const subject = `e2e-people-${hash.slice(0, 8)}`;
  const email = `people-${hash.slice(0, 8)}@e2e.wardyn.invalid`;
  sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
     VALUES (gen_random_uuid(), '${subject}', '${email}', 'user', 'standard', '[]'::jsonb, false, 'e2e', '${hash}', now())`,
  );
  const own = { Authorization: `Bearer ${raw}` };
  const created = await request.post("/api/v1/people", { headers: auth, data: { principal: subject, email } });
  expect(created.status(), await created.text()).toBeLessThan(300);
  const key = await request.post("/api/v1/me/ssh-keys", { headers: own, data: { name: "e2e-laptop", public_key: PUBLIC_KEY } });
  expect(key.status(), await key.text()).toBe(201);
  return { own, subject, email };
}

async function keyCount(request: import("@playwright/test").APIRequestContext, own: { Authorization: string }): Promise<number> {
  const res = await request.get("/api/v1/me/ssh-keys", { headers: own });
  expect(res.ok()).toBe(true);
  return ((await res.json()) ?? []).length;
}

test.describe.configure({ mode: "serial" });

test.describe("People (Admin view)", () => {
  test("an admin finds a person, signs them out everywhere and removes their SSH keys, each after a confirm", async ({ page }) => {
    const person = await seedPerson(page.request);
    await gotoConsole(page, "admin");
    await navTo(page, "People");
    await expect(page).toHaveURL(/\/admin\/people$/);
    await expect(page.getByRole("heading", { name: "People", level: 1 })).toBeVisible();
    await expect(page.getByText("Everyone who can reach this deployment, and what each person holds.")).toBeVisible();
    await expect(page.getByRole("heading", { name: "Who can sign in" })).toBeVisible();
    // The SCIM card is mounted here too (Settings keeps its own).
    await expect(page.getByRole("heading", { name: "SCIM provisioning" })).toBeVisible();

    await page.getByRole("textbox", { name: "Search people" }).fill(person.email.slice(0, 14));
    const row = page.getByRole("row").filter({ hasText: person.email });
    await expect(row).toBeVisible();
    await expect(row.getByText("Never signed in")).toBeVisible();
    await expect(row.getByText("1 key")).toBeVisible();
    await row.getByRole("button", { name: person.email }).click();

    const drawer = page.getByRole("dialog");
    await expect(drawer.getByRole("heading", { name: new RegExp(`^${person.email}`) })).toBeVisible();
    // The token list stays; minting for another person is refused server-side (#1477), so no control offers it.
    await expect(drawer.getByText("API tokens")).toBeVisible();
    await expect(drawer.getByRole("listitem").filter({ hasText: "e2e" })).toBeVisible();
    await expect(drawer.getByRole("button", { name: /mint/i })).toHaveCount(0);

    // Sign out everywhere: asks first, then sends exactly this person.
    let revoked: unknown = null;
    await page.route("**/api/v1/sessions/revoke", async (route) => {
      revoked = route.request().postDataJSON();
      await route.fulfill({ status: 204 });
    });
    await drawer.getByRole("button", { name: "Sign out everywhere" }).click();
    const confirm = page.getByRole("alertdialog");
    await expect(
      confirm.getByText(`Sign ${person.email} out of every browser and the CLI? They can sign in again unless their access is removed.`),
    ).toBeVisible();
    expect(revoked).toBeNull();
    await confirm.getByRole("button", { name: "Sign out everywhere" }).click();
    await expect(confirm).toHaveCount(0);
    // Only the sessions go: the route would otherwise revoke their tokens and delete their SSH keys too.
    expect(revoked).toEqual({ sub: person.subject, sessions_only: true });

    // Remove all SSH keys: the confirm changes nothing until it is accepted.
    await drawer.getByRole("button", { name: "Remove all" }).click();
    await expect(
      page.getByRole("alertdialog").getByText(`Remove every SSH key ${person.email} has added? Their open SSH sessions end.`),
    ).toBeVisible();
    await page.getByRole("alertdialog").getByRole("button", { name: "Cancel" }).click();
    expect(await keyCount(page.request, person.own)).toBe(1);

    await drawer.getByRole("button", { name: "Remove all" }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Remove all" }).click();
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    await expect.poll(() => keyCount(page.request, person.own)).toBe(0);
    await expect(drawer.getByRole("button", { name: "Remove all" })).toBeDisabled();

    const audit = await page.request.get(`/api/v1/audit?action=ssh_key.delete`, { headers: auth });
    expect(audit.ok()).toBe(true);
    const events = (await audit.json()) as { action: string; target?: string; resource?: string }[];
    expect(events.some((e) => e.action === "ssh_key.delete")).toBe(true);
  });

  test("a security admin sees who-can-sign-in as super-admin only, and still runs the per-person actions", async ({ page }) => {
    const person = await seedPerson(page.request);
    await asRealSecurityAdmin(page);
    await page.goto("/admin/people");
    await expect(page.getByRole("heading", { name: "People", level: 1 })).toBeVisible();
    await expect(sidebarPeople(page)).toBeVisible();
    await expect(page.getByText("Only the super admin can see and change who can sign in.")).toBeVisible();
    await page.getByRole("textbox", { name: "Search people" }).fill(person.email.slice(0, 14));
    await page.getByRole("button", { name: person.email }).click();
    await expect(page.getByRole("dialog").getByRole("button", { name: "Remove all" })).toBeEnabled();
    await expect(page.getByRole("dialog").getByRole("button", { name: "Sign out everywhere" })).toBeEnabled();
  });

  test("a member is refused at the route and has no People entry", async ({ page }) => {
    await asRealMember(page);
    await page.goto("/admin/people");
    await expect(page.getByText(/This page is part of the admin view/i)).toBeVisible();
    await expect(sidebarPeople(page)).toHaveCount(0);
  });
});

function sidebarPeople(page: import("@playwright/test").Page) {
  return page.getByRole("link", { name: /^People/ });
}
