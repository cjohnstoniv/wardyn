/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps capability card (plan slice S10) on the standalone
// /approvals list. The seeded e2e backend has no real Azure DevOps
// organisation to escalate against, so — same technique runs.spec.ts's
// "a failing side fetch is not an outage" describes and model-access-
// banner.spec.ts's mockModelAccess uses for a field the seeded backend
// doesn't produce — this spec intercepts **/api/v1/approvals* wholesale with
// a synthetic escalation row rather than seeding a real one, and lets
// everything else (auth, nav, GET /runs/{id} for RunContextRow) hit the real
// backend. Only a browser proves the caret-staged scope, the explicit
// decision_scope on the wire, and the card leaving the list once decided.
import { test, expect, gotoConsole, navTo, sql } from "./fixtures";
import { ADO } from "../src/app/lib/ado-entra-copy";

const APPROVAL_ID = "e2e-ado-escalation-1";

function escalationRow(runId: string) {
  return {
    id: APPROVAL_ID,
    run_id: runId,
    grant_id: "e2e-grant-1",
    kind: "tool_call",
    requested_scope: {
      lane: "azure_devops",
      provider_id: "e2e-row-1",
      org: "acme",
      grant_id: "e2e-grant-1",
      capability: "code_write",
      repo: "payments-api",
      ref_class: "",
      tool: "Azure DevOps",
      cmd: "Push commits and move branches that no policy protects (code_write) in acme/payments-api",
    },
    state: "PENDING",
    requested_at: new Date().toISOString(),
  };
}

test.describe("Approvals — the Azure DevOps capability card", () => {
  test("hold -> approve once -> the card leaves the pending list", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");

    // Flips true once the approve POST lands, so the very next PENDING list
    // fetch (ApprovalsScreen's own post-decide fetchAll()) reflects it —
    // exactly what the real backend does, without a real approvals table row.
    let decided = false;
    await page.route("**/api/v1/approvals*", async (route) => {
      const req = route.request();
      const url = new URL(req.url());
      if (req.method() === "GET") {
        const state = url.searchParams.get("state");
        const pending = !decided && (state === "PENDING" || state === "") ? [escalationRow(runId)] : [];
        await route.fulfill({ json: pending });
        return;
      }
      await route.continue();
    });
    let approveBody: Record<string, unknown> | null = null;
    await page.route(`**/api/v1/approvals/${APPROVAL_ID}/approve`, async (route) => {
      approveBody = route.request().postDataJSON() as Record<string, unknown>;
      decided = true;
      await route.fulfill({ json: { ...escalationRow(runId), state: "APPROVED", decision_scope: "once" } });
    });

    await gotoConsole(page);
    await navTo(page, "Approvals");

    const card = page.getByTestId("ado-capability-card");
    await expect(card).toBeVisible();
    // exact: true — "Push" and "acme/payments-api" both also appear as
    // SUBSTRINGS of the composed Command field's own text (F4 fix: a loose
    // getByText hit strict-mode multi-match against that field).
    await expect(card.getByRole("heading", { name: "Push", exact: true })).toBeVisible();
    await expect(card.getByText("acme/payments-api", { exact: true })).toBeVisible();

    // Stage "Once" from the caret before deciding — the scope readout tracks
    // it, unstaged, before either button is pressed.
    await card.getByRole("button", { name: "More options" }).click();
    await page.getByText("Once", { exact: true }).click();
    await expect(card.getByText(ADO.REQ_SCOPE_READOUT("Once"))).toBeVisible();

    await card.getByRole("button", { name: "Approve" }).click();

    await expect(card).toHaveCount(0);
    expect(approveBody).toMatchObject({ decision_scope: "once" });
  });
});
