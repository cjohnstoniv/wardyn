/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Approvals screen holds a card whose decision is out, or was just made, until the list is read again, and the
// list keeps the gap between its cards. Two seeding styles, both already used by this suite:
//   * the Waiting on you hold is spliced into the list route with a synthetic row (approvals-waiting-on-you.spec.ts),
//     because the seeded backend has no person who decides other people's holds;
//   * the other tests seed real pending approvals through SQL (approvals.spec.ts) and decide them for real, apart from
//     the 409, whose answer is spliced in.
// Every test mutates the one pending queue, so the file runs in order in one worker, as approvals.spec.ts does.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, gotoConsole, navTo, sql } from "./fixtures";

test.describe.configure({ mode: "serial" });

const anyRunId = () => {
  const id = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
  if (!id) throw new Error("no seeded runs found — is the backend up and seeded?");
  return id;
};

const clearPending = () => sql("DELETE FROM approvals WHERE state = 'PENDING'");
const deleteApproval = (id: string) => sql(`DELETE FROM approvals WHERE id = '${id}'`);

const uniqueMarker = (prefix: string) => `${prefix}-${randomUUID().slice(0, 8)}.example.test`;
const markerRe = (marker: string) => new RegExp(marker.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));

function seedPending(marker: string): string {
  const id = randomUUID();
  const scopeJson = JSON.stringify({ domain: marker, port: 443 });
  sql(
    `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
     VALUES ('${id}', '${anyRunId()}', 'egress_domain', '${scopeJson}'::jsonb, 'PENDING', now())`,
  );
  return id;
}

// A promise a route handler waits on, and the function that lets it go.
function gate() {
  let open!: () => void;
  const opened = new Promise<void>((resolve) => (open = resolve));
  return { opened, open };
}

// Every approval card is a fieldset the screen disables while a decision for it is out.
const cards = (page: Page) => page.locator("fieldset[data-focus-row]");

async function openApprovals(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Approvals");
  await expect(page.getByRole("heading", { name: "Approvals", level: 1 })).toBeVisible();
}

async function seedTwo(prefix: string) {
  clearPending();
  const markers = [uniqueMarker(`${prefix}-a`), uniqueMarker(`${prefix}-b`)];
  const ids = markers.map(seedPending);
  return { markers, ids };
}

test.describe("Approvals: a card with a decision out is held", () => {
  test("a card stays disabled through leaving Waiting on you and coming back, until the read after its decision answers", async ({ page }) => {
    const iso = new Date().toISOString();
    const row = {
      id: "e2e-held-1",
      run_id: anyRunId(),
      kind: "egress_domain",
      requested_scope: { host: "e2e-held-1.example.com" },
      state: "PENDING",
      requested_at: iso,
      requested_by: "dana.reyes@acme.example",
      deciders: ["owner", "unit_admins", "group:eng-leads", "security"],
      decidable: true,
    };
    let decided = false;
    const postGate = gate();
    const readGate = gate();
    const readHeld = gate();
    await asRealMember(page);
    // The Waiting on you list: once the decision has been answered, every read of it is held, then says the row is gone.
    await page.route(
      (url) => url.pathname.endsWith("/api/v1/approvals") && url.searchParams.get("decidable_by") === "me",
      async (route) => {
        if (decided) {
          readHeld.open();
          await readGate.opened;
        }
        await route.fulfill({ body: JSON.stringify(decided ? [] : [row]), contentType: "application/json" });
      },
    );
    // The decision's answer is held until the test lets it go.
    await page.route("**/api/v1/approvals/e2e-held-1/approve", async (route) => {
      await postGate.opened;
      decided = true;
      await route.fulfill({ body: JSON.stringify({ ...row, state: "APPROVED" }), contentType: "application/json" });
    });
    try {
      await openApprovals(page);
      const waiting = page.getByRole("tab", { name: /^Waiting on you/ });
      await waiting.click();
      const approve = page.getByRole("button", { name: "Approve", exact: true });
      const deny = page.getByRole("button", { name: "Deny", exact: true });
      await expect(approve).toBeEnabled();

      const posted = page.waitForRequest((r) => r.method() === "POST" && r.url().endsWith("/approvals/e2e-held-1/approve"));
      await approve.click();
      const dialog = page.getByRole("dialog");
      await dialog.getByRole("button", { name: "Approve", exact: true }).click();
      await posted;
      // The dialog's Cancel stays live while its decision is out; closing it is what frees the tabs behind it.
      await dialog.getByRole("button", { name: "Cancel" }).click();
      await expect(dialog).toHaveCount(0);
      await expect(approve).toBeDisabled();
      await expect(deny).toBeDisabled();

      // Away to another tab and back: the card is drawn again, and still held while the decision is out.
      await page.getByRole("tab", { name: /^Pending/ }).click();
      await waiting.click();
      await expect(approve).toBeDisabled();
      await expect(deny).toBeDisabled();

      // The decision is answered; the read after it is held, and the card is still off through that read.
      postGate.open();
      await readHeld.opened;
      await expect(approve).toBeDisabled();
      await expect(deny).toBeDisabled();
      await page.getByRole("tab", { name: /^Pending/ }).click();
      await waiting.click();
      await expect(approve).toBeDisabled();
      await expect(deny).toBeDisabled();

      // The read answers: the request is gone.
      readGate.open();
      await expect(approve).toHaveCount(0);
      await expect(page.getByText("Reach e2e-held-1.example.com", { exact: true })).toHaveCount(0);
    } finally {
      postGate.open();
      readGate.open();
    }
  });

  test("a decision from the reason dialog closes the dialog when the read answers, and focus lands on the next card", async ({ page }) => {
    const { markers, ids } = await seedTwo("focus-flow");
    try {
      await openApprovals(page);
      await expect(cards(page)).toHaveCount(2);
      // The list's own order decides which card is first.
      const firstText = await cards(page).first().innerText();
      const secondMarker = markerRe(markers[0]).test(firstText) ? markers[1] : markers[0];

      await cards(page).first().getByRole("button", { name: "Approve" }).click();
      const dialog = page.getByRole("dialog");
      await dialog.getByLabel(/Reason/).fill("Verified scope is minimal and time-boxed");
      await dialog.getByRole("button", { name: "Approve", exact: true }).click();
      await expect(dialog).toHaveCount(0);
      await expect(cards(page)).toHaveCount(1);
      await expect(page.getByText(markerRe(secondMarker)).first()).toBeVisible();

      const focused = page.locator(":focus");
      await expect(focused).toHaveCount(1);
      await expect(cards(page).first().locator(":focus")).toHaveCount(1);
      await expect(page.getByRole("heading", { name: "Approvals", level: 1 })).not.toBeFocused();
    } finally {
      ids.forEach(deleteApproval);
    }
  });

  test("an answer of already decided reads the list at once: the request leaves it and its reason dialog closes", async ({ page }) => {
    const { markers, ids } = await seedTwo("decided-elsewhere");
    try {
      await openApprovals(page);
      await expect(cards(page)).toHaveCount(2);
      const firstText = await cards(page).first().innerText();
      const [firstMarker, firstId] = markerRe(markers[0]).test(firstText) ? [markers[0], ids[0]] : [markers[1], ids[1]];

      // Someone else decided it first: the row is gone from the server's pending list, and this decision is refused.
      await page.route(`**/api/v1/approvals/${firstId}/approve`, async (route) => {
        deleteApproval(firstId);
        await route.fulfill({
          status: 409,
          contentType: "application/json",
          body: JSON.stringify({ error: "this request has already been decided", reason: "approval_already_decided" }),
        });
      });
      await cards(page).first().getByRole("button", { name: "Approve" }).click();
      const dialog = page.getByRole("dialog");
      await expect(dialog).toBeVisible();
      await dialog.getByRole("button", { name: "Approve", exact: true }).click();

      // Well inside the 10 s poll: the read came from the 409, not from the next tick.
      await expect(dialog).toHaveCount(0, { timeout: 4_000 });
      await expect(page.getByText(markerRe(firstMarker))).toHaveCount(0, { timeout: 4_000 });
      await expect(cards(page)).toHaveCount(1);
    } finally {
      ids.forEach(deleteApproval);
    }
  });
});

test.describe("Approvals: the list of cards", () => {
  test("two cards keep the 14px gap between them", async ({ page }) => {
    const { ids } = await seedTwo("gap");
    try {
      await openApprovals(page);
      await expect(cards(page)).toHaveCount(2);
      const box1 = await cards(page).nth(0).boundingBox();
      const box2 = await cards(page).nth(1).boundingBox();
      expect(box1).not.toBeNull();
      expect(box2).not.toBeNull();
      expect(box2!.y - (box1!.y + box1!.height)).toBeGreaterThanOrEqual(14);
    } finally {
      ids.forEach(deleteApproval);
    }
  });
});
