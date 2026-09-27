/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// User types (UT-7a) against the real backend: the list's built-in row says
// why it can't be deleted, and a type's "What this type gets" grid reads GET
// /permissions/explain (#739) — a type deny as Blocked with the wall note, a
// restricted image this type isn't listed for as Not available with who it is
// for, and Remove deleting this type's own row (read back from the wire).
// Every expected string comes from the copy modules, never retyped.
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo } from "./fixtures";
import { PERM } from "../src/app/lib/permissions-copy";
import { EXPLAIN, USER_TYPES as UT } from "../src/app/lib/user-types-copy";
import type { APIRequestContext, Page } from "@playwright/test";

test.describe.configure({ mode: "serial" });

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const ANALYST = { id: "e2e-analyst", name: "E2E analyst" };
const DEVELOPER = { id: "e2e-developer", name: "E2E developer" };
const IMAGE = "ghcr.io/acme/e2e-user-types:1.0";

async function post(request: APIRequestContext, path: string, data: object): Promise<void> {
  const res = await request.post(`/api/v1${path}`, { headers: auth, data });
  expect(res.status(), await res.text()).toBeLessThan(300);
}

async function grantsFor(request: APIRequestContext, subject: string) {
  const res = await request.get("/api/v1/permissions", { headers: auth });
  expect(res.status()).toBe(200);
  const body = (await res.json()) as { grants: { subject_type: string; subject: string; capability: string; value: string }[] };
  return body.grants.filter((g) => g.subject_type === "user_type" && g.subject === subject);
}

async function openType(page: Page, name: string): Promise<void> {
  await gotoConsole(page, "admin");
  await navTo(page, "User types");
  await expect(page.getByRole("heading", { name: UT.TITLE, level: 1 })).toBeVisible();
  await page.getByRole("button", { name: `${UT.EDIT} ${name}` }).click();
  await expect(page.getByRole("heading", { name: EXPLAIN.TITLE })).toBeVisible();
}

const cell = (page: Page, kind: string, value: string) => page.getByTestId(`explain-row-${kind}-${value}`);

test.describe("User types — the list and what each type gets", () => {
  test("seed: two types, a deny for one, an image only the other may use", async ({ request }) => {
    await post(request, "/user-types", ANALYST);
    await post(request, "/user-types", DEVELOPER);
    await post(request, "/permissions/grants", {
      subject_type: "user_type",
      subject: ANALYST.id,
      capability: "agent",
      value: "codex-cli",
      effect: "deny",
    });
    await post(request, "/permissions/grants", {
      subject_type: "user_type",
      subject: DEVELOPER.id,
      capability: "image",
      value: IMAGE,
      effect: "allow",
    });
    const res = await request.put(`/api/v1/permissions/availability/image/${IMAGE}`, {
      headers: auth,
      data: { restricted: true },
    });
    expect(res.status(), await res.text()).toBe(200);
  });

  test("the built-in type's Delete is disabled and says why in visible text", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "User types");
    const del = page.getByRole("button", { name: `${UT.DELETE} Standard user` });
    await expect(del).toBeDisabled();
    await expect(page.getByText(UT.DELETE_BUILTIN)).toBeVisible();
    await expect(page.getByRole("button", { name: `${UT.DELETE} ${ANALYST.name}` })).toBeEnabled();
  });

  test("a type's deny reads Blocked with the wall note; the image it isn't listed for is Not available, only the other type", async ({
    page,
  }) => {
    await openType(page, ANALYST.name);
    const blocked = cell(page, "agent", "codex-cli");
    await expect(blocked.getByText(EXPLAIN.STATE.blocked, { exact: true })).toBeVisible();
    await expect(page.getByText(EXPLAIN.WALL_HEAD)).toBeVisible();
    await expect(page.getByText(EXPLAIN.WALL_BODY)).toBeVisible();

    const image = cell(page, "image", IMAGE);
    await expect(image.getByText(EXPLAIN.STATE.not_available, { exact: true })).toBeVisible();
    await expect(image.getByText(EXPLAIN.ONLY(DEVELOPER.name))).toBeVisible();
    // Not this type's row: nothing to remove.
    await expect(image.getByRole("button")).toHaveCount(0);
  });

  test("the listed type gets the image as its own, and Remove deletes that row", async ({ page, request }) => {
    await openType(page, DEVELOPER.name);
    const image = cell(page, "image", IMAGE);
    await expect(image.getByText(EXPLAIN.STATE.this_type, { exact: true })).toBeVisible();

    await image.getByRole("button", { name: `${EXPLAIN.REMOVE} ${IMAGE}` }).click();
    await expect(page.getByText(PERM.REMOVE_CONFIRM(DEVELOPER.name))).toBeVisible();
    await page.getByRole("alertdialog").getByRole("button", { name: PERM.REMOVE }).click();

    // Still restricted, now listing nobody: the row stays, Not available.
    await expect(image.getByText(EXPLAIN.STATE.not_available, { exact: true })).toBeVisible();
    await expect(image.getByRole("button")).toHaveCount(0);
    expect(await grantsFor(request, DEVELOPER.id)).toEqual([]);
  });
});
