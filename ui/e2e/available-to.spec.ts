/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #923 — "Available to" against the real backend: on a base image (the Images
// tab, Admins only, Add image asking, a list refused after the image saved),
// on a stored policy (its sheet, New policy asking, and a person outside the
// list refused), and on a model provider (the editor asking). Each write is
// read back from the wire, not only from the screen.
import { createHash, randomBytes } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, navToRoute, sql } from "./fixtures";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import { AVAILABILITY, IMAGES } from "../src/app/lib/availability-copy";
import { MODEL_PROVIDERS, PROVIDER_EDITOR } from "../src/app/lib/model-providers-copy";
import type { Page } from "@playwright/test";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const LISTED = "ghcr.io/acme/e2e-toolbox:1.0";
const UNLISTED = "ghcr.io/acme/e2e-python-ml:3.12";
const PARTIAL = "ghcr.io/acme/e2e-partial:2.0";

async function gotoImages(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/admin/settings");
  await page.getByTestId("providers-card").getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
  await page.getByRole("button", { name: IMAGES.TAB, exact: true }).click();
}

async function availability(page: Page, ref: string) {
  const res = await page.request.get(`/api/v1/permissions/availability/image/${ref}`, { headers: auth });
  expect(res.status()).toBe(200);
  return res.json();
}

// The row holding one reference.
const row = (page: Page, ref: string) => page.getByTestId(`image-row-${ref}`);

test.describe("Available to — base images (#923)", () => {
  test.describe.configure({ mode: "serial" });

  test("Add image asks who gets it: Only these with a user type lands restricted, listed and locked", async ({ page }) => {
    await gotoImages(page);
    await page.getByRole("button", { name: IMAGES.ADD_CTA, exact: true }).click();

    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("heading", { name: IMAGES.ADD_CTA })).toBeVisible();
    // The form asks, starting at Admins only (design §2.6, decision 2).
    await expect(dialog.getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    await expect(dialog.getByText(AVAILABILITY.IMAGE_HINT)).toBeVisible();

    await dialog.getByLabel(IMAGES.REF).fill(LISTED);
    // "standard" is the built-in user type every deployment has (UT-1).
    await dialog.getByPlaceholder(AVAILABILITY.ADD_PLACEHOLDER).fill("standard");
    await dialog.getByRole("button", { name: AVAILABILITY.ADD_CTA, exact: true }).click();
    await expect(dialog.getByText("Standard user")).toBeVisible();
    await dialog.getByRole("radio", { name: AVAILABILITY.ONLY }).click();
    await dialog.getByRole("button", { name: IMAGES.ADD_CTA, exact: true }).click();
    await expect(dialog).toHaveCount(0);

    // The row, its control read back from the server.
    const r = row(page, LISTED);
    await expect(r.getByRole("radio", { name: AVAILABILITY.ONLY })).toBeChecked();
    await expect(r.getByText("Standard user")).toBeVisible();
    await expect(r.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Standard user") })).toBeDisabled();
    await expect(r.getByText(AVAILABILITY.LAST_AUDIENCE_LOCKED_IMAGE)).toBeVisible();
    await expect(r.getByText(AVAILABILITY.IMAGE_NOTE)).toBeVisible();

    const view = await availability(page, LISTED);
    expect(view.restricted).toBe(true);
    expect(view.allowed_by).toEqual([
      expect.objectContaining({ subject_type: "user_type", subject: "standard", capability: "image", value: LISTED }),
    ]);
  });

  test("an image added as Admins only stays admins-only: nothing listed, nothing restricted", async ({ page }) => {
    await gotoImages(page);
    await page.getByRole("button", { name: IMAGES.ADD_CTA, exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel(IMAGES.REF).fill(UNLISTED);
    await dialog.getByLabel(IMAGES.REF).press("Enter");
    await expect(dialog).toHaveCount(0);

    const r = row(page, UNLISTED);
    await expect(r.getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    await expect(r.getByText(AVAILABILITY.IMAGE_HINT)).toBeVisible();
    await expect(r.getByRole("radio", { name: AVAILABILITY.EVERYONE })).toHaveCount(0);

    const view = await availability(page, UNLISTED);
    expect(view.restricted).toBe(false);
    expect(view.allowed_by).toEqual([]);
    const catalog = await (await page.request.get("/api/v1/base-images", { headers: auth })).json();
    expect(catalog.base_images.map((b: { image: string }) => b.image)).toEqual(expect.arrayContaining([LISTED, UNLISTED]));
  });

  test("Only these on an image with nobody listed is refused as sent, and stays Admins only", async ({ page }) => {
    await gotoImages(page);
    const r = row(page, UNLISTED);
    await r.getByRole("radio", { name: AVAILABILITY.ONLY }).click();
    await expect(
      r.getByText("Add at least one person, group or user type before choosing Only, or nobody could use this."),
    ).toBeVisible();
    await expect(r.getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    expect((await availability(page, UNLISTED)).restricted).toBe(false);
  });

  test("the image saved but its list was refused: the dialog stays open on it, admins-only", async ({ page }) => {
    await gotoImages(page);
    await page.getByRole("button", { name: IMAGES.ADD_CTA, exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel(IMAGES.REF).fill(PARTIAL);
    await dialog.getByPlaceholder(AVAILABILITY.ADD_PLACEHOLDER).fill("portfolio-mgr");
    await dialog.getByRole("button", { name: AVAILABILITY.ADD_CTA, exact: true }).click();
    await dialog.getByRole("radio", { name: AVAILABILITY.ONLY }).click();
    await dialog.getByRole("button", { name: IMAGES.ADD_CTA, exact: true }).click();

    await expect(dialog.getByText(AVAILABILITY.CREATE_PARTIAL_TITLE)).toBeVisible();
    await expect(dialog.getByText(`The user type "portfolio-mgr" doesn't exist. Create it under User types first.`)).toBeVisible();
    await expect(dialog.getByText(AVAILABILITY.CREATE_PARTIAL_IMAGE)).toBeVisible();
    await expect(dialog.getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();

    const view = await availability(page, PARTIAL);
    expect(view.restricted).toBe(false);
    expect(view.allowed_by).toEqual([]);
    const catalog = await (await page.request.get("/api/v1/base-images", { headers: auth })).json();
    expect(catalog.base_images.map((b: { image: string }) => b.image)).toContain(PARTIAL);
  });
});

// A genuine user-tier caller. The harness's admin bearer is exempt from every
// capability, so a person outside a list is seeded the way the server's own
// mint stores one: an api_tokens row keyed by the token's sha256, with the
// role and user type the request then carries (apitokens.go, apiTokenAuth).
function seedUserToken(userType: string): { Authorization: string } {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const who = `${userType}-${hash.slice(0, 8)}@e2e.test`;
  sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
     VALUES (gen_random_uuid(), '${who}', '${who}', 'user', '${userType}', '[]'::jsonb, false, 'e2e', '${hash}', now())`,
  );
  return { Authorization: `Bearer ${raw}` };
}

const POLICY_NAME = "Read-only research";
const POLICY_SPEC = JSON.stringify({
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
  eligible_grants: [],
});

test.describe("Available to — a stored policy (#923)", () => {
  test.describe.configure({ mode: "serial" });
  let policyId = "";

  test("New policy asks who gets it: Only these with a user type lands restricted and listed", async ({ page }) => {
    // A second user type, so the built-in "standard" is the one left out.
    const made = await page.request.post("/api/v1/user-types", {
      headers: auth,
      data: { id: "portfolio-manager", name: "Portfolio manager" },
    });
    expect(made.status(), await made.text()).toBe(201);

    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await page.getByRole("button", { name: "New policy" }).first().click();
    const dialog = page.getByRole("dialog").filter({ hasText: "New policy" });
    await expect(dialog.getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    await expect(dialog.getByText(AVAILABILITY.POLICY_NOTE)).toBeVisible();
    await dialog.getByLabel("Name").fill(POLICY_NAME);
    await dialog.getByLabel("Spec (JSON)").fill(POLICY_SPEC);
    await dialog.getByPlaceholder(AVAILABILITY.ADD_PLACEHOLDER).fill("portfolio-manager");
    await dialog.getByRole("button", { name: AVAILABILITY.ADD_CTA, exact: true }).click();
    await expect(dialog.getByText("Portfolio manager")).toBeVisible();
    await dialog.getByRole("radio", { name: AVAILABILITY.ONLY }).click();
    await dialog.getByRole("button", { name: "Create policy" }).click();
    await expect(dialog).toHaveCount(0);

    const list = await (await page.request.get("/api/v1/policies", { headers: auth })).json();
    const rows = (Array.isArray(list) ? list : list.policies ?? list.items) as { id: string; name: string }[];
    policyId = rows.find((p) => p.name === POLICY_NAME)!.id;
    const view = await (
      await page.request.get(`/api/v1/permissions/availability/policy/${policyId}`, { headers: auth })
    ).json();
    expect(view.restricted).toBe(true);
    expect(view.allowed_by).toEqual([
      expect.objectContaining({ subject_type: "user_type", subject: "portfolio-manager", capability: "policy", value: policyId }),
    ]);
  });

  test("the policy's sheet shows its Available to: Only these, the type listed and locked, the policy's lines", async ({
    page,
  }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await page.getByText(POLICY_NAME, { exact: true }).click();
    const sheet = page.getByRole("dialog").filter({ hasText: "View raw JSON" });
    await expect(sheet.getByRole("radio", { name: AVAILABILITY.ONLY })).toBeChecked();
    await expect(sheet.getByTestId("availability-only")).toHaveText(AVAILABILITY.POLICY_ONLY_HINT);
    await expect(sheet.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Portfolio manager") })).toBeDisabled();
    await expect(sheet.getByText(AVAILABILITY.LAST_AUDIENCE_LOCKED)).toBeVisible();
    await expect(sheet.getByText(AVAILABILITY.POLICY_NOTE)).toBeVisible();
  });

  test("a person outside the list can't pick it; a person on the list can", async ({ page }) => {
    const outsider = seedUserToken("standard");
    const insider = seedUserToken("portfolio-manager");
    const run = (headers: { Authorization: string }) =>
      page.request.post("/api/v1/runs", {
        headers,
        data: { agent: "claude-code", repo: "acme/widgets", task: "e2e available-to policy", policy_id: policyId },
      });

    const refused = await run(outsider);
    const body = await refused.text();
    expect(refused.status(), body).toBe(403);
    expect(JSON.parse(body).error).toBe(
      `Stored policy ${policyId} isn't available to you. Ask your admin, or launch without policy_id.`,
    );
    // The listed type is not refused on the policy (whatever else its launch meets).
    expect(await (await run(insider)).text()).not.toContain("isn't available to you");
  });
});

test.describe("Available to — a model provider (#923)", () => {
  test("a new provider asks who gets it, and an existing one carries the live control", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await page.getByRole("button", { name: MODEL_PROVIDERS.ADD_CTA }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_api_key }).click();
    await expect(dialog.getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    await expect(dialog.getByTestId("availability-only")).toHaveText(AVAILABILITY.MODEL_PROVIDER_ONLY_HINT);
    await dialog.getByLabel(PROVIDER_EDITOR.NAME).fill("Bloomberg gateway");
    await dialog.getByPlaceholder(AVAILABILITY.ADD_PLACEHOLDER).fill("standard");
    await dialog.getByRole("button", { name: AVAILABILITY.ADD_CTA, exact: true }).click();
    await expect(dialog.getByText("Standard user")).toBeVisible();
    await dialog.getByRole("radio", { name: AVAILABILITY.ONLY }).click();
    await dialog.getByRole("button", { name: PROVIDER_EDITOR.SAVE, exact: true }).click();
    await expect(dialog).toHaveCount(0);

    const view = await (
      await page.request.get("/api/v1/permissions/availability/model_provider/bloomberg-gateway", { headers: auth })
    ).json();
    expect(view.restricted).toBe(true);
    expect(view.allowed_by).toEqual([
      expect.objectContaining({ subject_type: "user_type", subject: "standard", capability: "model_provider" }),
    ]);

    // Reopened, the saved provider shows what the server holds.
    await page.getByRole("button", { name: /^Bloomberg gateway/ }).click();
    const edit = page.getByRole("dialog");
    await expect(edit.getByRole("radio", { name: AVAILABILITY.ONLY })).toBeChecked();
    await expect(edit.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Standard user") })).toBeDisabled();
    await expect(edit.getByText(AVAILABILITY.MODEL_PROVIDER_NOTE)).toBeVisible();
  });
});
