/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2859 — the Approvals notes run the full width of their column, like the header description above
// them. A 72-character cap left the Azure DevOps note four lines tall and the host-wide note wrapping at
// half the card. Rows are synthetic (same technique as approvals-ado.spec.ts), so no approval is seeded.
import type { Locator } from "@playwright/test";
import { test, expect, gotoConsole, navTo, sql } from "./fixtures";
import { ADO } from "../src/app/lib/ado-entra-copy";
import { APPROVAL } from "../src/app/components/wardyn/copy";

// The note's width against its parent's content box: the column is the measure.
async function fillsParent(note: Locator): Promise<boolean> {
  return note.evaluate((el) => {
    const parent = el.parentElement as HTMLElement;
    const cs = getComputedStyle(parent);
    const content = parent.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
    return Math.abs(el.getBoundingClientRect().width - content) <= 1;
  });
}

test.describe("Approvals — notes use the column's width (#2859)", () => {
  test("the Azure DevOps note and the host-wide note are not capped at 72 characters", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    const rows = [
      {
        id: "e2e-notes-ado-1",
        run_id: runId,
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
          cmd: "Push commits and create or move branches (code_write) in acme/payments-api",
        },
        state: "PENDING",
        requested_at: new Date().toISOString(),
      },
      {
        id: "e2e-notes-egress-1",
        run_id: runId,
        kind: "egress_domain",
        requested_scope: { domain: "registry.npmjs.org", port: 443 },
        state: "PENDING",
        requested_at: new Date().toISOString(),
      },
    ];
    await page.route("**/api/v1/approvals*", async (route) => {
      if (route.request().method() === "GET") {
        const state = new URL(route.request().url()).searchParams.get("state");
        await route.fulfill({ json: state === "PENDING" || state === "" ? rows : [] });
        return;
      }
      await route.continue();
    });

    await gotoConsole(page);
    await navTo(page, "Approvals");

    const adoNote = page.getByText(ADO.TOOL_CALL_NOTE, { exact: true });
    await expect(adoNote).toBeVisible();
    await expect(adoNote).toHaveCSS("max-width", "none");
    expect(await fillsParent(adoNote)).toBe(true);

    const hostNote = page.getByText(APPROVAL.HOST_WIDE_NOTE, { exact: true });
    await expect(hostNote).toBeVisible();
    await expect(hostNote).toHaveCSS("max-width", "none");
    expect(await fillsParent(hostNote)).toBe(true);
  });
});
