/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The MCP call card on the standalone /approvals list. The seeded e2e backend has no MCP server to hold a call, so,
// as approvals-ado.spec.ts does, this spec intercepts **/api/v1/approvals* with a synthetic row and lets the rest hit
// the real backend. The wire is sent as raw text so the numbers stay as the server wrote them: adjacent integers past
// 2^53 and a decimal a JavaScript Number cannot keep. Only a browser proves the card shows them, copies them, and
// decides once with no scope on the wire.
import { test, expect, gotoConsole, navTo, sql } from "./fixtures";
import { APPROVAL_MCP_CALL as C } from "../src/app/components/wardyn/copy/approvals";

const APPROVAL_ID = "e2e-mcp-call-1";
const ARGS_TEXT = '{\n  "ids": [\n    9007199254740993,\n    9007199254740995\n  ],\n  "price": 1.0000000000000001\n}';

const wire = (runId: string) => `{"id":"${APPROVAL_ID}","run_id":"${runId}","kind":"tool_call","state":"PENDING","requested_at":"${new Date().toISOString()}",
"requested_scope":{"tool":"mcp__jira__delete_issue","mcp_server":"jira","mcp_host":"jira.example.com","arguments":{"ids":[9007199254740993,9007199254740995],"price":1.0000000000000001},"call_sha256":"${"ab".repeat(32)}"},
"call_text":{"arguments":${JSON.stringify(ARGS_TEXT)}},"decidable":true}`;

test.describe("Approvals — one MCP call", () => {
  test("shows the exact arguments, copies them, and approves once with no scope", async ({ page, context }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    let decided = false;
    await page.route("**/api/v1/approvals*", async (route) => {
      const req = route.request();
      if (req.method() === "GET") {
        const state = new URL(req.url()).searchParams.get("state");
        const pending = !decided && (state === "PENDING" || state === "") ? `[${wire(runId)}]` : "[]";
        await route.fulfill({ body: pending, contentType: "application/json" });
        return;
      }
      await route.continue();
    });
    let body: string | null = null;
    await page.route(`**/api/v1/approvals/${APPROVAL_ID}/approve`, async (route) => {
      body = route.request().postData();
      decided = true;
      await route.fulfill({ body: wire(runId).replace('"PENDING"', '"APPROVED"'), contentType: "application/json" });
    });

    await gotoConsole(page);
    await navTo(page, "Approvals");

    const card = page.getByTestId("mcp-call-card");
    await expect(card).toBeVisible();
    await expect(card.getByRole("region", { name: C.ARGUMENTS })).toHaveText(ARGS_TEXT);
    await expect(card).not.toContainText("9007199254740992");
    // No scope menu: one call is approved once.
    await expect(card.getByRole("button", { name: /more options/i })).toHaveCount(0);

    await card.getByRole("button", { name: C.COPY_ARGUMENTS }).click();
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(ARGS_TEXT);

    await card.getByRole("button", { name: C.APPROVE }).click();
    await expect(card).toHaveCount(0);
    expect(JSON.parse(body ?? "{}")).not.toHaveProperty("scope");
    expect(body ?? "").not.toMatch(/"(scope|decision_scope)"/);
  });
});
