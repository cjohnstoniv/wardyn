/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The held-push card (#181, built on #180/#494's server side) on the
// standalone /approvals list. Same splice technique approvals-ado.spec.ts
// uses: the seeded e2e backend has no real held push to raise, so
// **/api/v1/approvals* is intercepted wholesale with a synthetic
// push_content row rather than seeding a real one through a run's own proxy.
// Only a browser proves the round trip actually posts, and that a member
// sees the card without a decision control.
import { test, expect, gotoConsole, navTo, mockMemberRole, sql } from "./fixtures";
import { APPROVALS } from "../src/app/lib/approvals-copy";

const APPROVAL_ID = "e2e-held-push-1";

function pushRow(runId: string) {
  return {
    id: APPROVAL_ID,
    run_id: runId,
    kind: "push_content",
    requested_scope: {
      repo: "github.com/acme/payments-api",
      branch: "refs/heads/feature/checkout",
      acts_as: "github_token:11111111-1111-1111-1111-111111111111",
      paths: [".github/workflows/deploy.yml"],
      paths_total: 1,
      commits: ["deadbeef".repeat(5)],
      paths_digest: "a".repeat(64),
      acts_as_kind: "github_app",
      acts_as_label: "dana@acme.example",
    },
    state: "PENDING",
    requested_at: new Date().toISOString(),
  };
}

test.describe("Approvals — a held push (#181)", () => {
  test("hold -> approve -> the card leaves the pending list, with no decision_scope on the wire", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");

    let decided = false;
    await page.route("**/api/v1/approvals*", async (route) => {
      const req = route.request();
      const url = new URL(req.url());
      if (req.method() === "GET") {
        const state = url.searchParams.get("state");
        const pending = !decided && (state === "PENDING" || state === "") ? [pushRow(runId)] : [];
        await route.fulfill({ json: pending });
        return;
      }
      await route.continue();
    });
    let approveBody: Record<string, unknown> | null = null;
    await page.route(`**/api/v1/approvals/${APPROVAL_ID}/approve`, async (route) => {
      approveBody = route.request().postDataJSON() as Record<string, unknown>;
      decided = true;
      await route.fulfill({ json: { ...pushRow(runId), state: "APPROVED" } });
    });

    await gotoConsole(page);
    await navTo(page, "Approvals");

    const card = page.getByTestId("push-content-card");
    await expect(card).toBeVisible();
    await expect(card.getByText("A push is held for review", { exact: true })).toBeVisible();
    await expect(card.getByText("github.com/acme/payments-api", { exact: true })).toBeVisible();
    await expect(card.getByText(".github/workflows/deploy.yml", { exact: true })).toBeVisible();
    // acts_as_label, never the raw acts_as (a credential reference).
    await expect(card.getByText("dana@acme.example", { exact: true })).toBeVisible();
    await expect(card.getByText(/github_token:11111111/)).toHaveCount(0);

    await card.getByRole("button", { name: "Approve" }).click();

    // The canon toast every approval kind uses (#458), not a hand-typed one.
    await expect(page.getByText(APPROVALS.TOAST_APPROVED)).toBeVisible();
    await expect(card).toHaveCount(0);
    // No ReasonDialog, no decision_scope — decide's rule 4 refuses one on
    // this kind, and this card never builds one.
    expect(approveBody).not.toHaveProperty("decision_scope");
  });

  test("a member sees the full card but no decision control", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await mockMemberRole(page);
    await page.route("**/api/v1/approvals*", async (route) => {
      const req = route.request();
      if (req.method() === "GET") {
        const url = new URL(req.url());
        const state = url.searchParams.get("state");
        await route.fulfill({ json: state === "PENDING" || state === "" ? [pushRow(runId)] : [] });
        return;
      }
      await route.continue();
    });

    await gotoConsole(page);
    await navTo(page, "Approvals");

    const card = page.getByTestId("push-content-card");
    await expect(card).toBeVisible();
    await expect(card.getByText("github.com/acme/payments-api", { exact: true })).toBeVisible();
    await expect(card.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(card.getByRole("button", { name: "Deny" })).toHaveCount(0);
    await expect(card.getByText("Requires the admin or security admin role.", { exact: true })).toBeVisible();
  });
});
