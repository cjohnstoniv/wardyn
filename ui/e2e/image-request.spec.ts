/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Asking for an image on Approvals, following it under "Your requests", and the image row on Governance ›
// Changes. The seeded tokens carry no groups and the seeded profiles no image rule, so the request list and the
// create are spliced with synthetic rows, as elevation-request.spec.ts does, and everything else hits the real
// backend. The Changes rows are spliced the way governance-changes-requests.spec.ts does.
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, asRealSecurityAdmin, consoleAPI, gotoConsole, navTo } from "./fixtures";
import { CHANGES } from "../src/app/lib/governance-copy";
import { ELEV } from "../src/app/components/wardyn/copy/elevation";
import { CHANGE_NOTE } from "../src/app/components/wardyn/copy/change-note";
import { IMG } from "../src/app/components/wardyn/copy/image-request";

const ahead = (hours: number) => new Date(Date.now() + hours * 3_600_000).toISOString();
const payments = { unit_id: "u1", unit_name: "Payments", max_elevation: "4h" };
const HEX = "4f2a91c3" + "0".repeat(56);
const NAME = "ghcr.io/acme/build-tools";
const REF = `${NAME}@sha256:${HEX}`;
const REASON = "The nightly build needs the new toolchain INC-2210";

const imageRow = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  target_kind: "governance_image_request",
  op: "create",
  target_key: `e2e-member|sha256:${HEX}`,
  state: "pending",
  kind: "image",
  image: REF,
  unit_name: "Payments",
  proposed_by: "e2e-member",
  proposed_at: ahead(-0.05),
  expires_at: ahead(24),
  diff: { before: null, after: null, changed: [] },
  mine: true,
  decidable: false,
  deciders: [{ kind: "unit_admin", unit_name: "Payments" }, { kind: "security" }],
  ...extra,
});

const elevationRow = (id: string, extra: Record<string, unknown> = {}) => ({
  ...imageRow(id),
  target_kind: "governance_elevation",
  target_key: "u1",
  kind: "elevation",
  image: undefined,
  unit_id: "u1",
  duration: "4h",
  ...extra,
});

const listPath = (url: URL) => url.pathname.endsWith("/api/v1/governance/requests");

async function serveList(page: Page, body: { requests?: unknown[]; offered?: unknown[]; image_offered?: boolean }) {
  await page.route(listPath, (route) =>
    route.request().method() === "GET"
      ? route.fulfill({
          contentType: "application/json",
          body: JSON.stringify({ requests: [], offered: [], image_offered: false, ...body }),
        })
      : route.fallback(),
  );
}

async function serveCreate(page: Page, status: number, body: unknown) {
  await page.route(listPath, (route) =>
    route.request().method() === "POST" ? route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) }) : route.fallback(),
  );
}

async function openApprovals(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Approvals");
}

const dialog = (page: Page) => page.getByRole("dialog");
const imageField = (page: Page) => dialog(page).locator("textarea.font-mono");
const reasonField = (page: Page) => page.getByLabel(ELEV.REASON_LABEL);
const sendButton = (page: Page) => dialog(page).getByRole("button", { name: IMG.SEND });
const card = (page: Page) => page.locator("section", { has: page.getByText(ELEV.LIST_TITLE, { exact: true }) }).first();

async function openImageDialog(page: Page) {
  await page.getByRole("button", { name: IMG.ACTION }).click();
  await expect(dialog(page)).toBeVisible();
}

async function sendRefused(page: Page, status: number, reason: string, error: string, ref = REF) {
  await serveList(page, { image_offered: true });
  await serveCreate(page, status, { error, reason });
  await openApprovals(page);
  await openImageDialog(page);
  await imageField(page).fill(ref);
  await reasonField(page).fill(REASON);
  await sendButton(page).click();
}

test.describe("Approvals: image requests", () => {
  test("nothing offered: no Your requests card", async ({ page }) => {
    await asRealMember(page);
    const read = await consoleAPI(page, "GET", "/api/v1/governance/requests");
    expect(read.status, read.text).toBe(200);
    const body = JSON.parse(read.text);
    expect(body.image_offered).toBe(false);
    expect(body.requests ?? []).toEqual([]);
    await openApprovals(page);
    await expect(page.getByRole("tab", { name: /^Pending/ })).toBeVisible();
    await expect(page.getByText(ELEV.LIST_TITLE)).toHaveCount(0);
  });

  test("images only: the card offers Request an image, says Images by digest. and You have no requests.", async ({ page }) => {
    await asRealMember(page);
    await serveList(page, { image_offered: true });
    await openApprovals(page);
    await expect(page.getByText(ELEV.LIST_TITLE, { exact: true })).toBeVisible();
    await expect(page.getByText(IMG.LIST_LEAD_IMAGES, { exact: true })).toBeVisible();
    await expect(page.getByText(IMG.LIST_EMPTY, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: ELEV.ACTION })).toHaveCount(0);
    await openImageDialog(page);
    await expect(sendButton(page)).toBeDisabled();
    await imageField(page).fill(REF);
    await expect(sendButton(page)).toBeDisabled();
    await reasonField(page).fill(REASON);
    await expect(sendButton(page)).toBeEnabled();
    await expect(dialog(page).getByRole("radiogroup", { name: IMG.KIND_LABEL })).toHaveCount(0);
  });

  test("both kinds: one card, both actions; the dialog has the kind choice, and Elevation shows the elevation fields", async ({ page }) => {
    await asRealMember(page);
    await serveList(page, { offered: [payments], image_offered: true });
    await openApprovals(page);
    await expect(page.getByText(IMG.LIST_LEAD_BOTH, { exact: true })).toBeVisible();
    await expect(card(page).getByRole("button", { name: ELEV.ACTION })).toBeVisible();
    await openImageDialog(page);
    const choice = dialog(page).getByRole("radiogroup", { name: IMG.KIND_LABEL });
    await expect(choice).toBeVisible();
    await expect(dialog(page).getByText(IMG.TITLE, { exact: true })).toBeVisible();
    await choice.getByText(IMG.KIND_ELEVATION, { exact: true }).click();
    await expect(dialog(page).getByText(ELEV.TITLE, { exact: true })).toBeVisible();
    await expect(dialog(page).getByRole("combobox", { name: ELEV.UNIT_LABEL })).toBeVisible();
    await expect(dialog(page).getByRole("radiogroup", { name: IMG.KIND_LABEL })).toBeVisible();
  });

  test("a tag is refused under Image and the fields keep their text", async ({ page }) => {
    await asRealMember(page);
    const sentence = "request an image by digest: <name>@sha256:<64 hex>";
    await sendRefused(page, 400, "image_request_not_pinned", sentence, `${NAME}:latest`);
    await expect(dialog(page).getByText(sentence, { exact: true })).toBeVisible();
    await expect(imageField(page)).toHaveValue(`${NAME}:latest`);
    await expect(imageField(page)).toHaveAttribute("aria-invalid", "true");
    await expect(reasonField(page)).toHaveValue(REASON);
  });

  test("already granted and already waiting", async ({ page }) => {
    await asRealMember(page);
    await sendRefused(page, 409, "image_already_granted", `${REF} is already granted to you`);
    await expect(dialog(page)).toContainText(ELEV.REFUSED_TITLE);
    await expect(dialog(page).getByRole("alert")).toContainText(`${REF} is already granted to you`);
    await expect(reasonField(page)).toHaveValue(REASON);

    await page.unroute(listPath);
    await serveList(page, { image_offered: true });
    await serveCreate(page, 409, { error: "a change to this target is already waiting for approval: c-77", reason: "governance_change_pending" });
    await sendButton(page).click();
    await expect(dialog(page).getByRole("alert")).toContainText(IMG.ALREADY_WAITING);
    await expect(dialog(page)).not.toContainText("c-77");
    await expect(imageField(page)).toHaveValue(REF);
  });

  test("not offered on send, and deciders unreadable", async ({ page }) => {
    await asRealMember(page);
    const off = "image grants are off in this deployment, so an approved image would not launch; ask an admin";
    const norule = "your default unit offers no image requests; ask an admin for an image grant";
    await sendRefused(page, 400, "image_request_not_offered", off);
    await expect(dialog(page).getByRole("alert")).toContainText(off);
    for (const [status, reason, sentence] of [
      [400, "image_request_not_offered", norule],
      [503, "deciders_unreadable", "the deciders rules could not be read; try again"],
    ] as const) {
      await page.unroute(listPath);
      await serveList(page, { image_offered: true });
      await serveCreate(page, status, { error: sentence, reason });
      await sendButton(page).click();
      await expect(dialog(page).getByRole("alert")).toContainText(sentence);
      await expect(imageField(page)).toHaveValue(REF);
    }
  });

  test("sent names who decides", async ({ page }) => {
    await asRealMember(page);
    await serveList(page, { image_offered: true });
    const created = imageRow("e2e-img-1");
    await serveCreate(page, 202, { pending_change: created, request: created });
    await openApprovals(page);
    await openImageDialog(page);
    await imageField(page).fill(REF);
    await reasonField(page).fill(REASON);
    await sendButton(page).click();
    await expect(page.getByText(ELEV.SENT_TITLE)).toBeVisible();
    await expect(dialog(page)).toContainText("It is waiting for admins of Payments or a security admin.");
  });

  test("Your requests lists every image state beside an approved elevation", async ({ page }) => {
    await asRealMember(page);
    const me = "e2e-member";
    await serveList(page, {
      offered: [payments],
      image_offered: true,
      requests: [
        imageRow("e2e-img-wait", { proposed_at: ahead(-0.1) }),
        imageRow("e2e-img-ok", { state: "applied", proposed_at: ahead(-1), decided_by: "priya.nair", decided_at: ahead(-0.9) }),
        imageRow("e2e-img-no", { state: "rejected", proposed_at: ahead(-2), decided_by: "priya.nair", decided_at: ahead(-1.9), reason: "Use the pinned base image." }),
        imageRow("e2e-img-stale", { state: "stale", proposed_at: ahead(-3) }),
        imageRow("e2e-img-gone", { state: "rejected", proposed_at: ahead(-4), decided_by: me, decided_at: ahead(-3.9) }),
        imageRow("e2e-img-late", { state: "expired", proposed_at: ahead(-30), expires_at: ahead(-6) }),
        elevationRow("e2e-elev-ok", { state: "applied", ends_at: ahead(3.7), proposed_at: ahead(-0.5), decided_by: "priya.nair", decided_at: ahead(-0.4) }),
        elevationRow("e2e-elev-gone", { state: "rejected", proposed_at: ahead(-5), decided_by: me, decided_at: ahead(-4.9) }),
        elevationRow("e2e-elev-stale", { state: "stale", proposed_at: ahead(-6) }),
      ],
    });
    await openApprovals(page);
    const c = card(page);
    await expect(c.getByText(ELEV.STATE_WAITING, { exact: true })).toHaveCount(1);
    await expect(c.getByText(ELEV.STATE_APPROVED, { exact: true })).toHaveCount(2);
    await expect(c.getByText(ELEV.STATE_DENIED, { exact: true })).toHaveCount(1);
    await expect(c.getByText(IMG.STATE_STALE, { exact: true })).toHaveCount(2);
    await expect(c.getByText(IMG.STATE_WITHDRAWN, { exact: true })).toHaveCount(2);
    await expect(c.getByText(ELEV.STATE_EXPIRED, { exact: true })).toHaveCount(1);
    await expect(c.getByText(/\(in [23] h/)).toHaveCount(1);
    await expect(c.getByText(/\d+m\)/)).toHaveCount(0);
    await expect(c.getByText("sha256:4f2a91c3…", { exact: true })).toHaveCount(6);

    const posted = page.waitForRequest(
      (r) => r.method() === "POST" && new URL(r.url()).pathname.endsWith("/api/v1/governance/requests/e2e-img-wait/reject"),
    );
    await page.route("**/api/v1/governance/requests/e2e-img-wait/reject", (route) =>
      route.fulfill({ contentType: "application/json", body: JSON.stringify(imageRow("e2e-img-wait", { state: "rejected", decided_by: me })) }),
    );
    await expect(c.getByRole("button", { name: ELEV.WITHDRAW })).toHaveCount(1);
    await c.getByRole("button", { name: ELEV.WITHDRAW }).click();
    await posted;
  });

  test("the card's read failed shows Retry", async ({ page }) => {
    await asRealMember(page);
    let reads = 0;
    await page.route(listPath, (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      reads += 1;
      return reads === 1
        ? route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) })
        : route.fulfill({ contentType: "application/json", body: JSON.stringify({ requests: [], offered: [], image_offered: true }) });
    });
    await openApprovals(page);
    await expect(page.getByText(IMG.LIST_FAILED_TITLE, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: IMG.ACTION })).toHaveCount(0);
    await card(page).getByRole("button", { name: ELEV.RETRY }).click();
    await expect(page.getByText(IMG.LIST_EMPTY, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: IMG.ACTION })).toBeVisible();
  });
});

// The viewer is a security admin; `proposed_by` decides whether a row is theirs.
const VIEWER = "e2e-security-admin";

function imageChange(id: string, proposedBy: string) {
  return {
    id,
    target_kind: "governance_image_request",
    op: "create",
    target_key: `dana.reyes|sha256:${HEX}`,
    state: "pending",
    proposed_by: proposedBy,
    proposed_at: ahead(-1),
    expires_at: ahead(23),
    diff: { after: { image: REF, unit_id: "u1", unit_name: "Payments", subject: "dana.reyes" }, changed: ["image"] },
    note: { reason: "The nightly build needs the new toolchain", ticket: "INC-2210" },
  };
}

async function openChanges(page: Page, list: object[]) {
  await page.route("**/api/v1/governance/changes*", (route) => (route.request().method() === "GET" ? route.fulfill({ json: list }) : route.fallback()));
  await asRealSecurityAdmin(page);
  await gotoConsole(page, "admin");
  await navTo(page, "Governance");
  await page.getByRole("tab", { name: CHANGES.TAB_COUNT(list.length) }).click();
}

test.describe("governance: image requests on the Changes tab", () => {
  test("Changes shows the image row and drawer", async ({ page }) => {
    const label = `${CHANGES.KIND.governance_image_request} · ${CHANGES.OP.create}`;
    const sentence =
      "the target, or the deployment default, has changed since this change was proposed: it was not applied and is now stale; propose it again";
    await page.route("**/api/v1/governance/changes/*/approve", (route) =>
      route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: sentence, reason: "governance_change_stale" }) }),
    );
    await openChanges(page, [
      imageChange("00000000-0000-4000-8000-0000000000f1", "dana.reyes"),
      imageChange("00000000-0000-4000-8000-0000000000f2", VIEWER),
    ]);
    const rows = page.getByRole("row").filter({ has: page.getByRole("button", { name: label, exact: true }) });
    await expect(rows.first()).toContainText(IMG.CHANGE_LINE("INC-2210"));
    await expect(rows.first()).toContainText("dana.reyes → build-tools@sha256:4f2a91c3…");

    // Another person's request: five rows, then Approve answered stale.
    await page.getByRole("button", { name: label, exact: true }).first().click();
    const drawer = page.getByRole("dialog");
    for (const [name, value] of [
      [IMG.ROW_PERSON, "dana.reyes"],
      [IMG.ROW_IMAGE, NAME],
      [IMG.ROW_DIGEST, `sha256:${HEX}`],
      [IMG.ROW_ASKED_UNDER, IMG.ASKED_UNDER("Payments")],
      [IMG.ROW_WRITES, IMG.WRITES],
    ]) {
      await expect(drawer.getByRole("row").filter({ has: page.getByRole("cell", { name, exact: true }) })).toContainText(value);
    }
    await expect(drawer.getByRole("button", { name: CHANGES.REJECT, exact: true })).toBeEnabled();
    await drawer.getByRole("button", { name: CHANGES.APPROVE, exact: true }).click();
    await expect(drawer.getByText(CHANGES.DECIDE_REFUSED_TITLE, { exact: true })).toBeVisible();
    await expect(drawer.getByText(sentence, { exact: true })).toBeVisible();
    await page.keyboard.press("Escape");

    // The viewer's own request: Withdraw request, and Approve off.
    await page.getByRole("button", { name: label, exact: true }).nth(1).click();
    const own = page.getByRole("dialog");
    await expect(own.getByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST, exact: true })).toBeEnabled();
    await expect(own.getByText(CHANGE_NOTE.OWN_REQUEST, { exact: true })).toBeVisible();
    await expect(own.getByRole("button", { name: CHANGES.APPROVE, exact: true })).toBeDisabled();
  });
});
