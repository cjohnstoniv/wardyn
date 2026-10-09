/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// E2E for Audit → Retention, the drop flow and the by-scope person erasure
// (mock packet M4, approved 2026-10-03): audit-retention.tsx,
// audit-retention-drop-dialog.tsx, erase-data-dialog.tsx.
//
// Real backend and a real security_admin session throughout. The one stub is
// the drop's SUCCESS: a fresh e2e database was converted a moment ago, so its
// legacy partition cannot be older than the shortest retention window (one
// day) and the server would refuse any real drop. The server's refusal is
// real, though: the retention read is fetched from the daemon and only its
// eligibility is flipped, so the first Drop attempt hits the real route and
// shows the real refusal; the second answers 200 from the stub. A real drop is
// pinned by the store's Postgres suite (internal/store/auditretention_pg_test.go).
import { createHash, randomBytes } from "node:crypto";
import type { Page, Route } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, asRealMember, asRealSecurityAdmin, gotoConsole, navTo, sql } from "./fixtures";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

async function openRetention(page: Page): Promise<void> {
  await gotoConsole(page, "admin");
  await navTo(page, "Audit");
  await expect(page.getByRole("heading", { name: "Audit", exact: true })).toBeVisible();
  await page.getByRole("tab", { name: "Retention" }).click();
  await expect(page.getByText("Kept for")).toBeVisible();
}

test.describe.configure({ mode: "serial" });

test.describe("Audit retention (security admin)", () => {
  test("shows the policy and the partitions, with no Drop on a partition the server will not drop", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await openRetention(page);

    // A fresh install keeps the log forever and was converted just now.
    await expect(page.getByText("Forever")).toBeVisible();
    await expect(page.getByText(/^Events before \d+ \w+ \d{4} are in one partition/)).toBeVisible();
    await expect(page.getByText(/^\d+ months of partitions ready$/)).toBeVisible();

    const legacy = page.getByRole("row").filter({ hasText: "audit_events_legacy" });
    await expect(legacy).toContainText(/Before \d+ \w+ \d{4}/);
    await expect(legacy).toContainText("Closed");
    // Retention is forever, so the server's own answer is "inside the period".
    await expect(legacy).toContainText("Inside the retention period");
    await expect(legacy.getByRole("button", { name: "Drop" })).toHaveCount(0);

    // The current month's partition is still receiving events: nothing to export or drop.
    const open = page.getByRole("row").filter({ hasText: "Open" }).first();
    await expect(open.getByRole("button")).toHaveCount(0);
  });

  test("a closed partition exports as a raw archive whose footer carries the digest", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await openRetention(page);

    const legacy = page.getByRole("row").filter({ hasText: "audit_events_legacy" });
    await legacy.getByRole("button", { name: /Export/ }).click();
    const [download] = await Promise.all([
      page.waitForEvent("download"),
      page.getByRole("menuitem", { name: "Raw archive" }).click(),
    ]);
    expect(download.suggestedFilename()).toBe("audit_events_legacy.raw.ndjson");
    const body = await (await import("node:fs/promises")).readFile((await download.path())!, "utf8");
    const lines = body.trim().split("\n");
    expect(JSON.parse(lines[0]).type).toBe("manifest");
    expect(JSON.parse(lines[lines.length - 1]).digest).toMatch(/^[0-9a-f]{64}$/);
  });

  test("a drop: the server's real refusal shows its sentence, then the accepted drop shows its result", async ({ page }) => {
    await asRealSecurityAdmin(page);

    // The daemon's own answer, with the legacy partition flipped to eligible.
    const real = await page.request.get("/api/v1/audit/retention", { headers: auth });
    expect(real.status(), await real.text()).toBe(200);
    const status = await real.json();
    status.partitions[0].eligible = true;
    delete status.partitions[0].refusal;
    await page.route("**/api/v1/audit/retention", (route: Route) => route.fulfill({ json: status }));
    await openRetention(page);

    const legacy = page.getByRole("row").filter({ hasText: "audit_events_legacy" });
    await expect(legacy).toContainText("Can be dropped");
    await legacy.getByRole("button", { name: "Drop" }).click();

    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(/^Drop every event before \d+ \w+ \d{4}\?$/)).toBeVisible();
    const confirm = dialog.getByRole("button", { name: "Drop partition" });
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel("Digest from the archive's footer").fill("0".repeat(64));
    await expect(confirm).toBeEnabled();

    // Real route: retention is forever on this install, so the server refuses.
    await confirm.click();
    await expect(dialog.getByRole("alert")).toHaveText("This partition is still inside the retention period.");
    await expect(confirm).toBeVisible();

    // The accepted drop.
    await page.route("**/api/v1/audit/retention/drop", (route: Route) =>
      route.fulfill({
        status: 200,
        json: { partition: "audit_events_legacy", rows: 1234, seq_lo: 1, seq_hi: 1234, digest: "0".repeat(64), event_seq: 1235 },
      }),
    );
    await confirm.click();
    await expect(dialog.getByText("Dropped 1,234 events.")).toBeVisible();
    await expect(dialog.getByText("Recorded in the Audit log as audit.retention.partition_dropped.")).toBeVisible();
    await dialog.getByRole("button", { name: "Close" }).first().click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
  });

  test("a member does not see the Retention tab, and the route refuses them", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page, "user");
    await expect(page.getByRole("tab", { name: "Retention" })).toHaveCount(0);
    const res = await page.request.get("/api/v1/audit/retention");
    expect([401, 403]).toContain(res.status());
  });
});

// A person with a stored secret and an erasable run task, the way
// credentials.spec.ts seeds one: a distinct subject and email on a real
// api_tokens row.
function seedPerson(): { auth: { Authorization: string }; subject: string; email: string } {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const subject = `e2e-erase-subject-${hash.slice(0, 8)}`;
  const email = `erase-data-${hash.slice(0, 8)}@e2e.wardyn.invalid`;
  sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
     VALUES (gen_random_uuid(), '${subject}', '${email}', 'user', 'standard', '[]'::jsonb, false, 'e2e', '${hash}', now())`,
  );
  return { auth: { Authorization: `Bearer ${raw}` }, subject, email };
}

test.describe("Erase someone's data (security admin)", () => {
  test("erases a person's chosen scopes end to end, with Recordings opt-in and the result per scope", async ({ page }) => {
    const person = seedPerson();
    const put = await page.request.put("/api/v1/secrets/e2e-erase-secret", {
      headers: { ...person.auth, "Content-Type": "application/json" },
      data: { value: "e2e-erase-secret-value-0123456789" },
    });
    expect(put.status(), await put.text()).toBe(204);

    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Credentials");
    await page.getByRole("button", { name: "Erase someone's data" }).click();

    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Erase someone's data", { exact: true }).first()).toBeVisible();
    // Everything but Recordings is ticked on open; Recordings stays opt-in.
    for (const label of ["Credentials", "Personal details in audit events", "Run tasks", "Run output", "Saved components", "Copies kept for masking"]) {
      await expect(dialog.getByRole("checkbox", { name: new RegExp(`^${label}`) })).toBeChecked();
    }
    await expect(dialog.getByRole("checkbox", { name: /^Recordings/ })).not.toBeChecked();

    const confirm = dialog.getByRole("button", { name: "Erase data" });
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel("Email or subject").fill(person.email);
    await expect(confirm).toBeEnabled();
    await confirm.click();

    // The real route answers per scope; the dialog names each one it ran.
    await expect(dialog.getByText(`Erased the chosen data for ${person.email}.`)).toBeVisible();
    await expect(dialog.getByText("Recorded in the Audit log as person.erasure.")).toBeVisible();
    await expect(dialog.getByText("Erased", { exact: true })).toHaveCount(6);
    await expect(dialog.getByText("Recordings")).toHaveCount(0);
    await dialog.getByRole("button", { name: "Close" }).first().click();

    // It landed server-side: the person's secret is gone and the act is on the record.
    const secret = await page.request.get("/api/v1/secrets", { headers: person.auth });
    expect(JSON.stringify(await secret.json())).not.toContain("e2e-erase-secret");
    const audit = await page.request.get("/api/v1/audit?action=person.erasure", { headers: auth });
    const rows = (await audit.json()) as { items?: { target?: string; outcome: string }[] } | { target?: string; outcome: string }[];
    const list = Array.isArray(rows) ? rows : (rows.items ?? []);
    expect(list.some((e) => e.target === person.subject && e.outcome === "success")).toBe(true);
  });

  test("ticking Recordings sends it, and a refusal shows the server's own sentence", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Credentials");
    await page.getByRole("button", { name: "Erase someone's data" }).click();

    const dialog = page.getByRole("dialog");
    await dialog.getByRole("checkbox", { name: /^Recordings/ }).click();
    const sent = page.waitForRequest((r) => r.url().includes("/erasure") && r.method() === "POST");
    await dialog.getByLabel("Email or subject").fill("nobody-at-all@e2e.wardyn.invalid");
    await dialog.getByRole("button", { name: "Erase data" }).click();
    expect((await sent).postDataJSON().scopes).toContain("recordings");
    await expect(dialog.getByRole("alert")).toContainText(/doesn't match anyone this deployment knows/i);
    await expect(dialog.getByRole("button", { name: "Erase data" })).toBeVisible();
  });

  test("a member never gets the surface", async ({ page }) => {
    await asRealMember(page);
    await page.goto("/admin/credentials");
    await expect(page.getByRole("button", { name: "Erase someone's data" })).toHaveCount(0);
  });
});
