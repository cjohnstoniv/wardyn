/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, expandCard, gotoConsole, navTo, navToRoute } from "./fixtures";
import type { Page } from "@playwright/test";
import { GOVERNANCE as GOV, PERM, PREVIEW } from "../src/app/lib/governance-copy";
import { aheadByHours } from "../src/app/lib/test-clock";
import { SESSION_EXPIRES_HEADER } from "../src/app/lib/api/session-activity";

// Screens that must not act on an answer, or fail to act on a refusal, that no longer belongs to what is on
// screen: the Governance previews and delete confirms. The snapshot is stubbed at its route; the writes are held
// or refused by the stub so the race is the test, not the timing.

const CEILING = { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" };
const profile = (id: string, name: string) => ({
  id, name, ceiling: CEILING, limits: {}, created_at: aheadByHours(-1), updated_at: aheadByHours(-1),
});
const PLATFORM = profile("p-platform", "Platform");
const GREENFIELD = profile("p-greenfield", "Greenfield contractors");
const ASSIGNMENT = {
  id: "a-platform", subject_type: "group", subject: "wardyn.platform", unit_id: PLATFORM.id, priority: 10, created_at: aheadByHours(-1),
};

async function openGovernance(page: Page, state: { profiles: unknown[]; assignments: unknown[] }): Promise<{ reads: () => number }> {
  let reads = 0;
  await page.route("**/api/v1/governance", (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    reads += 1;
    return route.fulfill({ json: { units: state.profiles, memberships: state.assignments } });
  });
  await page.route("**/api/v1/governance/changes", (route) => route.fulfill({ json: [] }));
  await gotoConsole(page, "admin");
  await navTo(page, "Governance");
  await expect(page.getByRole("heading", { name: GOV.TITLE, level: 1 })).toBeVisible();
  return { reads: () => reads };
}

test.describe("Governance: an answer or a refusal is acted on once, and only for what is on screen", () => {
  test("the unassign confirm is disabled while its delete is in flight", async ({ page }) => {
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    let deletes = 0;
    await page.route("**/api/v1/governance/memberships/a-platform", async (route) => {
      deletes += 1;
      await held;
      await route.fulfill({ status: 204, body: "" });
    });
    await openGovernance(page, { profiles: [PLATFORM], assignments: [ASSIGNMENT] });

    await page.getByRole("button", { name: `${PERM.REMOVE} wardyn.platform` }).click();
    const confirm = page.getByRole("alertdialog").getByRole("button", { name: PERM.REMOVE, exact: true });
    await confirm.click();
    await expect(confirm).toBeDisabled();
    release();
    expect(deletes).toBe(1);
  });

  test("a refused profile delete re-reads the list", async ({ page }) => {
    await page.route("**/api/v1/governance/units/p-greenfield", (route) =>
      route.fulfill({ status: 409, json: { error: "this governance profile is still assigned", reason: "governance_unit_in_use" } }),
    );
    const governance = await openGovernance(page, { profiles: [GREENFIELD], assignments: [] });
    const before = governance.reads();

    await page.getByRole("button", { name: `${GOV.DELETE} ${GREENFIELD.name}` }).click();
    const dialog = page.getByRole("alertdialog");
    await dialog.getByRole("button", { name: GOV.DELETE, exact: true }).click();
    await expect(dialog.getByText(GOV.DELETE_RESTRICT_TITLE)).toBeVisible();
    await expect.poll(governance.reads).toBe(before + 1);
  });

  test("an answer to claims that were edited while it was in flight is not shown under the new claims", async ({ page }) => {
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    await page.route("**/api/v1/governance/check", async (route) => {
      await held;
      await route.fulfill({ json: { unit_id: PLATFORM.id, unit_name: PLATFORM.name, matched_tier: "group" } });
    });
    await openGovernance(page, { profiles: [PLATFORM], assignments: [] });

    const claims = page.getByLabel(PREVIEW.FIELD_CLAIMS);
    await claims.fill("wardyn.platform");
    const answered = page.waitForResponse((r) => r.url().includes("/governance/check"));
    await page.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }).click();
    await claims.fill("wardyn.other");
    release();
    await answered;
    await page.evaluate(() => new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done))));
    await expect(page.getByText(GOV.PREVIEW_RESULT(PLATFORM.name, GOV.MATCHED_GROUP))).toHaveCount(0);
  });
});

test.describe("Components: a delete is sent once, and its dialog holds until it answers", () => {
  test("Escape and a second click during a held delete neither dismiss nor re-send", async ({ page }) => {
    const component = {
      id: "11111111-1111-4111-8111-111111111111",
      name: "Payments API",
      version: 1,
      created_at: aheadByHours(-24),
      updated_at: aheadByHours(-24),
      definition: { hosts: ["api.pay.example"] },
    };
    let gone = false;
    await page.route("**/api/v1/components", (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: gone ? [] : [component] }) : route.fallback(),
    );
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    let deletes = 0;
    await page.route(`**/api/v1/components/${component.id}`, async (route) => {
      if (route.request().method() !== "DELETE") return route.fallback();
      deletes += 1;
      await held;
      gone = true;
      await route.fulfill({ status: 204, body: "" });
    });
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/components");

    await page.getByRole("button", { name: "Delete Payments API" }).click();
    const dialog = page.getByRole("alertdialog");
    const confirm = dialog.getByRole("button", { name: "Delete component" });
    await confirm.click();
    await expect(confirm).toBeDisabled();
    await page.keyboard.press("Escape");
    await expect(dialog).toBeVisible();
    release();
    await expect(dialog).toHaveCount(0);
    expect(deletes).toBe(1);
  });
});

const inMinutes = (m: number) => new Date(Date.now() + m * 60_000).toISOString();
const nextFrames = (page: Page) => page.evaluate(() => new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done))));

test.describe("Session expiry: answers are ordered by when their request started", () => {
  test("a slower, older answer naming an earlier expiry does not bring the expiry banner back", async ({ page }) => {
    // The first answer (a preview, held) names an expiry two minutes out; a later request (the Permissions
    // read) names an hour out and answers first. The held answer then lands: it is older, so it changes nothing.
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    let started!: () => void;
    const previewStarted = new Promise<void>((resolve) => (started = resolve));
    await page.route("**/api/v1/governance/check", async (route) => {
      started();
      await held;
      await route.fulfill({
        headers: { [SESSION_EXPIRES_HEADER.toLowerCase()]: inMinutes(2) },
        json: { unit_id: PLATFORM.id, unit_name: PLATFORM.name, matched_tier: "group" },
      });
    });
    await page.route("**/api/v1/permissions*", async (route) => {
      const response = await route.fetch();
      const headers = { ...response.headers(), [SESSION_EXPIRES_HEADER.toLowerCase()]: inMinutes(60) };
      delete headers["content-length"];
      await route.fulfill({ response, headers });
    });
    await openGovernance(page, { profiles: [PLATFORM], assignments: [] });

    await page.getByLabel(PREVIEW.FIELD_CLAIMS).fill("wardyn.platform");
    const previewAnswered = page.waitForResponse((r) => r.url().includes("/governance/check"));
    await page.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }).click();
    await previewStarted;
    const permissionsAnswered = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/v1/permissions");
    await navTo(page, "Permissions");
    await permissionsAnswered;
    release();
    await previewAnswered;
    await nextFrames(page);
    await expect(page.getByText("Your session is expiring soon.")).toHaveCount(0);
  });
});

test.describe("Admin settings: an older read cannot overwrite the newer one", () => {
  test("the Host card keeps saying the proxy is in use after an older Re-check lands late", async ({ page }) => {
    // The console holds every route behind its own first /setup/status, so the Settings screen is only ever
    // mounted once that read has landed. Holding that read (as this pin once did) just delays the whole page and
    // never reaches the screen's own reads. The race the screen guards is two of ITS loads in flight at once,
    // which Re-check makes: the first Re-check's site-config answer is held and reads "no proxy", the second
    // answers at once with the proxy, and the held one lands last.
    let counting = false;
    let reads = 0;
    let secondAsked!: () => void;
    const secondRead = new Promise<void>((resolve) => (secondAsked = resolve));
    let releaseOlder!: () => void;
    const olderHeld = new Promise<void>((resolve) => (releaseOlder = resolve));
    let olderLanded = false;
    const proxied = { upstream_proxy_url: "http://proxy.corp.example:3128" };
    await page.route("**/api/v1/site-config", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (!counting) return route.fulfill({ json: proxied });
      reads += 1;
      if (reads === 2) secondAsked();
      if (reads !== 1) return route.fulfill({ json: proxied });
      await olderHeld;
      await route.fulfill({ json: {} });
      olderLanded = true;
    });
    await page.goto("/admin/settings");
    await expect(page.getByRole("heading", { name: "Admin SSH keys" })).toBeVisible();
    await expandCard(page, "Host");
    await expect(page.getByTestId("host-tile-internet-pill")).toHaveText("Through the corporate proxy");
    const recheck = page.getByRole("button", { name: "Re-check" });
    counting = true;
    await recheck.click();
    await expect.poll(() => reads).toBe(1);
    await recheck.click();
    await secondRead;
    await nextFrames(page);
    await expect(page.getByTestId("host-tile-internet-pill")).toHaveText("Through the corporate proxy");
    releaseOlder();
    await expect.poll(() => olderLanded).toBe(true);
    await nextFrames(page);
    await expect(page.getByTestId("host-tile-internet-pill")).toHaveText("Through the corporate proxy");
  });
});
