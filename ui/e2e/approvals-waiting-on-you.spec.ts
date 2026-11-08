/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Approvals screen's "Waiting on you" tab and the deciders lines. The seeded e2e backend has no person who
// decides other people's holds (its tokens carry no groups), so, as approvals-mcp-call.spec.ts does, the decider
// states are spliced into the list routes with a synthetic row and everything else hits the real backend. The
// probe that tells a server that knows `decidable_by` from an older one is real.
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, asRealSecurityAdmin, consoleAPI, gotoConsole, navTo, sql } from "./fixtures";
import { DECIDERS_INBOX } from "../src/app/components/wardyn/copy/deciders-inbox";

const runId = () => sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
const iso = () => new Date().toISOString();

const pendingRow = (id: string, run: string, extra: Record<string, unknown> = {}) => ({
  id,
  run_id: run,
  kind: "egress_domain",
  requested_scope: { host: `${id}.example.com` },
  state: "PENDING",
  requested_at: iso(),
  ...extra,
});
const othersRow = (id: string, run: string, extra: Record<string, unknown> = {}) =>
  pendingRow(id, run, {
    requested_by: "dana.reyes@acme.example",
    deciders: ["owner", "unit_admins", "group:eng-leads", "security"],
    decidable: true,
    ...extra,
  });

const waitingTab = (page: Page) => page.getByRole("tab", { name: /^Waiting on you/ });

async function serveWaiting(page: Page, rows: unknown[], headers: Record<string, string> = {}) {
  await page.route(
    (url) => url.pathname.endsWith("/api/v1/approvals") && url.searchParams.get("decidable_by") === "me",
    (route) => route.fulfill({ body: JSON.stringify(rows), contentType: "application/json", headers }),
  );
}

async function openApprovals(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Approvals");
}

test.describe("Approvals — Waiting on you", () => {
  test("a security admin gets no Waiting on you tab", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await openApprovals(page);
    await expect(page.getByRole("tab", { name: /^Pending/ })).toBeVisible();
    await expect(waitingTab(page)).toHaveCount(0);
  });

  test("a member on a server that knows decidable_by, with nothing of others' to decide, gets no tab", async ({ page }) => {
    await asRealMember(page);
    const probe = await consoleAPI(page, "GET", "/api/v1/approvals?decidable_by=x");
    expect(probe.status, probe.text).toBe(400);
    expect(JSON.parse(probe.text).reason).toBe("invalid_decidable_by");
    await openApprovals(page);
    await expect(page.getByRole("tab", { name: /^Pending/ })).toBeVisible();
    await expect(waitingTab(page)).toHaveCount(0);
  });

  test("a decider sees the tab with its count, the Requested by row, no Open run link, and who decides", async ({ page }) => {
    await asRealMember(page);
    const run = runId();
    await serveWaiting(page, [othersRow("e2e-waiting-1", run), othersRow("e2e-waiting-2", run, { second_human: true })]);
    await openApprovals(page);
    await expect(waitingTab(page)).toHaveText(/Waiting on you\s*2/);
    await expect(page.getByText(DECIDERS_INBOX.HELP)).toBeVisible();
    await waitingTab(page).click();
    await expect(page.getByText("dana.reyes@acme.example")).toHaveCount(2);
    await expect(page.getByText(run.slice(0, 8)).first()).toBeVisible();
    await expect(page.getByText(DECIDERS_INBOX.CANT_OPEN_RUN)).toHaveCount(2);
    await expect(page.getByRole("link", { name: /open run/i })).toHaveCount(0);
    await expect(page.getByText(DECIDERS_INBOX.DECIDED_BY_LABEL).first().locator("..")).toContainText(
      "the run's owner, admins of this unit, the eng-leads group or a security admin",
    );
    await expect(page.getByText(DECIDERS_INBOX.SECOND_PERSON)).toHaveCount(1);
  });

  test("a list cut at 500 says so above the cards", async ({ page }) => {
    await asRealMember(page);
    await serveWaiting(page, [othersRow("e2e-waiting-1", runId())], { "X-Wardyn-Truncated": "true" });
    await openApprovals(page);
    await waitingTab(page).click();
    await expect(page.getByText(DECIDERS_INBOX.TRUNCATED)).toBeVisible();
    await expect(page.getByText("Reach e2e-waiting-1.example.com")).toBeVisible();
  });

  test("a refused decision reads as its own toast", async ({ page }) => {
    await asRealMember(page);
    await serveWaiting(page, [othersRow("e2e-waiting-1", runId())]);
    await page.route("**/api/v1/approvals/e2e-waiting-1/approve", (route) =>
      route.fulfill({
        status: 403,
        contentType: "application/json",
        body: JSON.stringify({ error: "only the run's owner or an admin of this unit decides this", reason: "not_a_decider" }),
      }),
    );
    await openApprovals(page);
    await waitingTab(page).click();
    await page.getByRole("button", { name: "Approve", exact: true }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Approve", exact: true }).click();
    await expect(page.getByText(DECIDERS_INBOX.TOAST_NOT_A_DECIDER)).toBeVisible();
  });

  test("decided rows say on what basis they were decided", async ({ page }) => {
    await asRealMember(page);
    const run = runId();
    const unit = "11111111-1111-1111-1111-111111111111";
    const decided = (id: string, basis?: string) =>
      pendingRow(id, run, { state: "APPROVED", decided_by: "dana.reyes", decided_at: iso(), ...(basis ? { decision_basis: basis } : {}) });
    const rows = [
      decided("e2e-d-unit", `unit_admin:${unit}`),
      decided("e2e-d-group", "group:eng-leads"),
      decided("e2e-d-security", "security"),
      decided("e2e-d-policy", "policy"),
      decided("e2e-d-owner", "owner"),
      decided("e2e-d-token", "admin_token"),
      decided("e2e-d-none"),
    ];
    await page.route(
      (url) => url.pathname.endsWith("/api/v1/approvals") && !url.searchParams.has("decidable_by"),
      (route, request) =>
        route.fulfill({
          body: JSON.stringify(new URL(request.url()).searchParams.get("state") === "APPROVED" ? rows : []),
          contentType: "application/json",
        }),
    );
    await page.route("**/api/v1/governance", (route) =>
      route.fulfill({ body: JSON.stringify({ units: [], unit_names: { [unit]: "Payments" } }), contentType: "application/json" }),
    );
    await openApprovals(page);
    await page.getByRole("tab", { name: /^Decided/ }).click();
    await expect(page.getByText("Decided as an admin of Payments")).toBeVisible();
    await expect(page.getByText("Decided as a member of the eng-leads group")).toBeVisible();
    await expect(page.getByText("Decided as a security admin")).toBeVisible();
    await expect(page.getByText("Decided by an approval rule, not a person")).toBeVisible();
    await expect(page.getByText("Decided as the run's owner")).toBeVisible();
    await expect(page.getByText("Decided with the admin token")).toBeVisible();
    await expect(page.getByText(/^Decided (as|by|with) /)).toHaveCount(6);
  });

  test("the Approve dialog disables scopes wider than the rule's cap", async ({ page }) => {
    await asRealMember(page);
    await page.route(
      (url) => url.pathname.endsWith("/api/v1/approvals") && url.searchParams.get("state") === "PENDING",
      (route) =>
        route.fulfill({
          body: JSON.stringify([pendingRow("e2e-capped-1", runId(), { deciders: ["owner"], max_scope: "until", decidable: true })]),
          contentType: "application/json",
        }),
    );
    await openApprovals(page);
    await page.getByRole("button", { name: "Approve", exact: true }).click();
    const dialog = page.getByRole("dialog");
    const thisRun = dialog.getByRole("radio", { name: /^This run/ });
    await expect(thisRun).toBeDisabled();
    await expect(thisRun).toContainText(DECIDERS_INBOX.SCOPE_CAP("Until"));
    await expect(dialog.getByRole("radio", { name: /^Always/ })).toBeDisabled();
    await expect(dialog.getByRole("radio", { name: /^Until/ })).toBeEnabled();
  });
});
