/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R4 F030/F031 — the workspace-detail egress surface spans TWO route tiers,
// and the console must not offer a control the server will refuse.
//
//   PUT /workspaces/{id}/approved-egress  -> securityOps  (a security admin holds it)
//   PUT /workspaces/{id}/requirements     -> operatorOnly (they do NOT)
//
// A security admin therefore gets the Allowed-hosts remove for a plain
// approved host, but NOT for one an operator-authored requirements row backs
// (removing that one is both writes), and NOT the Record pane's Approve-host
// controls (approveHosts is one requirements PUT for N hosts) — even though
// the pane as a whole is open to them.
//
// Intercept-driven exactly like record-loop.spec.ts: WS_ID is synthetic, the
// workspace GET answers from a literal, and mockSecurityAdminRole splices
// GET /me the way the member/security specs already do. This pins the RENDER
// contract of the tier split; the server-side refusal itself is pinned in Go
// (internal/api's authz route matrix).
import { test, expect, gotoConsole, navToRoute, mockSecurityAdminRole } from "./fixtures";
import type { Page } from "@playwright/test";

const WS_ID = "e2e-egress-tiers-ws";
const WS_GLOB = `**/api/v1/workspaces/${WS_ID}`;

const OPERATOR_SET = { level: "required", provenance: "operator_set" };

// registry.npmjs.org is approved AND carries an operator-authored requirements
// row (two writes to remove); pypi.org is approved only (one write).
// "build & test" replayed confined and caught one held host.
function workspaceFixture() {
  return {
    id: WS_ID,
    name: "egress-tiers-e2e",
    kind: "repo",
    source: "acme/egress-tiers",
    status: "scanned",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    approved_egress: ["registry.npmjs.org", "pypi.org"],
    requirements: { "egress:registry.npmjs.org": OPERATOR_SET },
    record_results: {
      "build-test": {
        run_id: "run-a",
        label: "build & test",
        mode: "interactive",
        status: "recorded",
        observations: { domains: [], minted_grant_ids: [], exec_argv0s: [], file_writes: [], connects: [], anomalies: [] },
      },
      "verify:build-test": {
        run_id: "run-b",
        label: "build & test",
        mode: "interactive",
        confined: true,
        status: "recorded",
        clean: false,
        caught: 1,
        finished_at: "2026-01-01T00:05:00Z",
        observations: {
          domains: [{ host: "files.pythonhosted.org", methods: ["GET"], allow_count: 0, deny_count: 0, pending_count: 1 }],
        },
      },
    },
  };
}

async function openDetail(page: Page) {
  await page.route(WS_GLOB, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(workspaceFixture()) }),
  );
  await gotoConsole(page);
  await navToRoute(page, `/workspaces/${WS_ID}`);
  await expect(page.getByRole("heading", { name: "egress-tiers-e2e" })).toBeVisible();
}

test.describe("Workspace detail — the two egress tiers a security admin straddles (F030/F031)", () => {
  test("a security admin keeps the one-write remove and loses the two-write one", async ({ page }) => {
    await mockSecurityAdminRole(page);
    await openDetail(page);

    // Approved only -> one securityOps write -> live.
    await expect(page.getByRole("button", { name: "Remove pypi.org" })).toBeEnabled();
    // Approved AND operator_set-required -> the second write is operatorOnly.
    await expect(page.getByRole("button", { name: "Remove registry.npmjs.org" })).toBeDisabled();
  });

  test("a security admin sees the Record pane open but both Approve-host controls dead", async ({ page }) => {
    await mockSecurityAdminRole(page);
    await openDetail(page);

    const session = page.getByTestId("session-build-test");
    await expect(session).toContainText("Replayed — caught 1");
    // The pane is OPEN to this tier — the selection checkbox proves it.
    await expect(session.getByRole("checkbox", { name: "Approve files.pythonhosted.org" })).toBeEnabled();
    // ...but approveHosts writes .../requirements, which they are refused.
    await expect(session.getByRole("button", { name: /^approve$/i })).toBeDisabled();
    await expect(session.getByRole("button", { name: /^approve 1 selected host and replay again$/i })).toBeDisabled();
  });

  test("an admin, who holds both tiers, sees every one of those controls live", async ({ page }) => {
    await openDetail(page);

    await expect(page.getByRole("button", { name: "Remove pypi.org" })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Remove registry.npmjs.org" })).toBeEnabled();
    const session = page.getByTestId("session-build-test");
    await expect(session.getByRole("button", { name: /^approve$/i })).toBeEnabled();
    await expect(session.getByRole("button", { name: /^approve 1 selected host and replay again$/i })).toBeEnabled();
  });
});
