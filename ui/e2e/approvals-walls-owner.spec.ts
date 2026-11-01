/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A request the run owner's walls deny names the owner by display name, not by sign-in subject (#3785), in the
// sentence and on the Why card. The seeded backend raises no walls refusal and has no directory, so the approval
// list, the run's owner and the directory search are spliced, as approvals-push.spec.ts splices its held push.
import { test, expect, gotoConsole, navTo, sql } from "./fixtures";

const APPROVAL_ID = "e2e-walls-owner-1";
const OWNER = "ana@example.com";

test.describe("Approvals — whose walls", () => {
  test("a refusal by another person's walls says their display name", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    const row = {
      id: APPROVAL_ID,
      run_id: runId,
      kind: "egress_domain",
      requested_scope: { host: "paste.example.net" },
      state: "PENDING",
      requested_at: new Date().toISOString(),
      walls_block: true,
    };
    await page.route("**/api/v1/approvals*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const state = new URL(route.request().url()).searchParams.get("state");
      await route.fulfill({ json: state === "PENDING" || state === "" ? [row] : [] });
    });
    await page.route(new RegExp(`/api/v1/runs/${runId}(\\?.*)?$`), async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const res = await route.fetch();
      await route.fulfill({ response: res, json: { ...(await res.json()), created_by: OWNER } });
    });
    await page.route("**/api/v1/access/directory/search*", (route) =>
      route.fulfill({ json: { results: [{ display_name: "Ana Torres", claim_value: OWNER, kind: "user" }] } }),
    );

    await gotoConsole(page);
    await navTo(page, "Approvals");

    const note = page.getByTestId("approval-walls-block");
    await expect(note).toContainText("Ana Torres's walls block paste.example.net for this run.");
    await expect(note).toContainText("OwnerAna Torres");
    await expect(note).not.toContainText(OWNER);
  });
});
