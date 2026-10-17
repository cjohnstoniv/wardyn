/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole } from "./fixtures";
import { POOL_DRAWER, RUNNER_TRUST } from "../src/app/components/wardyn/copy/runner-pools";

// Admin -> Runners trust class (0.9.3, owner mock item 3, Q5). The harness has no registered runner, so the
// runner routes are answered here with a person's runner and an organization's; what is pinned is the
// console: the edit with its required reason, a change held for a second person that changes nothing, and
// the read-only organization row. The server's own rules (the 422, the 202, the audit row) are pinned in
// the Go suite beside the route.
const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
function view(over: Record<string, unknown>) {
  return {
    state: "claimed",
    online: true,
    key_fingerprint: "SHA256:fp",
    key_fingerprint_abbreviated: false,
    created_at: "2026-10-10T10:00:00Z",
    claimed_at: "2026-10-10T11:00:00Z",
    kind: "self_hosted",
    limits: null,
    usage: { online: true, in_use: none, available: none, disk_capped: true },
    pools: [],
    ...over,
  };
}
const PERSON_ID = "00000000-0000-4000-8000-0000000000a1";
const PERSON = view({ id: PERSON_ID, name: "dana-laptop", owner: "dana@example.com", owner_kind: "person", trust_class: "personal" });
const ORG = view({ id: "00000000-0000-4000-8000-0000000000b2", name: "eks-prod-runners", owner_kind: "org", trust_class: "org_cluster", trust_set_by: "sam@example.com", trust_set_at: "2026-10-14T10:00:00Z" });

// answer: what PUT trust gives back; the list follows a 200 so the re-read shows the new class.
async function mockRunners(page: Page, answer: "applied" | "held") {
  const puts: unknown[] = [];
  let person: Record<string, unknown> = PERSON;
  await page.route("**/api/v1/runners/settings", (route) => route.fulfill({ json: { enabled: true } }));
  await page.route("**/api/v1/runners/tokens**", (route) => route.fulfill({ json: [] }));
  await page.route("**/api/v1/remote-runners", (route) => route.fulfill({ json: [] }));
  await page.route("**/api/v1/runners/*/trust", async (route) => {
    const body = route.request().postDataJSON() as { trust_class: string; reason: string };
    puts.push(body);
    if (answer === "held") {
      return route.fulfill({ status: 202, json: { pending_change: { id: "c1", target_kind: "runner_trust", op: "update", target_key: PERSON_ID, state: "pending", diff: { changed: ["trust_class"] } } } });
    }
    person = { ...person, trust_class: body.trust_class, trust_set_by: "e2e-admin", trust_set_at: new Date().toISOString() };
    return route.fulfill({ json: person });
  });
  await page.route(/\/api\/v1\/runners\?/, (route) => route.fulfill({ json: [person, ORG] }));
  return puts;
}

async function openRunner(page: Page, name: string) {
  await page.goto("/admin/runners");
  await page.getByRole("button", { name, exact: true }).click();
  return page.getByRole("dialog");
}

test("a security admin changes a person's runner to Managed device, with a reason", async ({ page }) => {
  const puts = await mockRunners(page, "applied");
  await gotoConsole(page, "admin");
  const drawer = await openRunner(page, "dana-laptop");
  const save = drawer.getByRole("button", { name: RUNNER_TRUST.SAVE });
  await expect(drawer.getByRole("combobox", { name: RUNNER_TRUST.TITLE })).toHaveText(RUNNER_TRUST.PERSONAL);
  await expect(save).toBeDisabled();
  await drawer.getByRole("combobox", { name: RUNNER_TRUST.TITLE }).click();
  await page.getByRole("option", { name: RUNNER_TRUST.MANAGED }).click();
  await expect(drawer.getByText(RUNNER_TRUST.MANAGED_DESC)).toBeVisible();
  // The reason is required: a changed class alone does not enable the button.
  await expect(save).toBeDisabled();
  await drawer.getByLabel(RUNNER_TRUST.REASON_LABEL).fill("Enrolled in device management, ticket SEC-2210");
  await save.click();
  await expect(page.getByText(RUNNER_TRUST.DONE("dana-laptop", RUNNER_TRUST.MANAGED))).toBeVisible();
  expect(puts).toEqual([{ trust_class: "managed_device", reason: "Enrolled in device management, ticket SEC-2210" }]);
  // The re-read shows the new class and who set it.
  await expect(drawer.getByRole("combobox", { name: RUNNER_TRUST.TITLE })).toHaveText(RUNNER_TRUST.MANAGED);
  await expect(drawer.getByText(/^Set by e2e-admin /)).toBeVisible();
});

test("a change held for a second person says so and changes nothing", async ({ page }) => {
  const puts = await mockRunners(page, "held");
  await gotoConsole(page, "admin");
  const drawer = await openRunner(page, "dana-laptop");
  await drawer.getByRole("combobox", { name: RUNNER_TRUST.TITLE }).click();
  await page.getByRole("option", { name: RUNNER_TRUST.MANAGED }).click();
  await drawer.getByLabel(RUNNER_TRUST.REASON_LABEL).fill("Enrolled");
  await drawer.getByRole("button", { name: RUNNER_TRUST.SAVE }).click();
  await expect(drawer.getByText(POOL_DRAWER.HELD, { exact: true })).toBeVisible();
  await expect(drawer.getByText(POOL_DRAWER.HELD_BODY, { exact: true })).toBeVisible();
  expect(puts).toHaveLength(1);
  // Nothing was announced as done, and the runner's card behind the drawer still carries no Managed device chip.
  await expect(page.getByText(RUNNER_TRUST.DONE("dana-laptop", RUNNER_TRUST.MANAGED))).toHaveCount(0);
  const card = page.locator('li[aria-label="dana-laptop"]');
  await expect(card).toBeVisible();
  await expect(card.getByText(RUNNER_TRUST.MANAGED)).toHaveCount(0);
  // The drawer's Save is off while the proposal waits.
  await expect(drawer.getByRole("button", { name: RUNNER_TRUST.SAVE })).toBeDisabled();
});

test("an organization's runner shows Organization cluster and offers no control", async ({ page }) => {
  await mockRunners(page, "applied");
  await gotoConsole(page, "admin");
  const drawer = await openRunner(page, "eks-prod-runners");
  await expect(drawer.getByText(RUNNER_TRUST.ORG_DESC)).toBeVisible();
  await expect(drawer.getByText(/^Set by sam@example\.com /)).toBeVisible();
  await expect(drawer.getByRole("combobox")).toHaveCount(0);
  await expect(drawer.getByLabel(RUNNER_TRUST.REASON_LABEL)).toHaveCount(0);
  await expect(drawer.getByRole("button", { name: RUNNER_TRUST.SAVE })).toHaveCount(0);
});
