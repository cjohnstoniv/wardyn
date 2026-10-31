/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, sql, asRealMember, asRealSecurityAdmin, MEMBER_PRINCIPAL } from "./fixtures";

// ============================================================================
// /open/{run}/{app} (docs/UI-SANDBOXES.md, "Open it in a browser") against the
// SEEDED backend: the link people are given for an inbound app. The page asks
// the server for the caller's ticket, binds it and enters, in the same tab.
//
// This backend shares ONE origin for every run (no WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE) and its `none`
// runner has no sandbox, so the cases here are the ones that need neither: who gets which sentence, and that
// the owner's hand-off leaves the console for the gateway. A named caller who is not the owner, landing in a
// real app on a per-run host, is proven live by scripts/run-e2e-ui-sandbox.sh (its host-mode section).
//
// The seeded RUNNING run declares a plain `vscode` app. This file adds the envelope dispatch would write
// for a run that also declares an inbound `api` app naming the seeded member; the last envelope wins
// (latestPolicyResolve), and each spec file gets a freshly seeded backend, so no other spec sees it.
// ============================================================================

const RUN_TASK = "e2e fixture 2";
const NOT_SHARED = "This app isn't shared with you, or the run no longer exists.";
const SHARED_HOST =
  "This deployment serves apps from one shared host, so only the run's owner can open them in a browser. Ask an administrator to set per-run hosts.";

const POLICY = JSON.stringify({
  allowed_domains: [],
  first_use_approval: "always_deny",
  min_confinement_class: "CC1",
  ui_apps: [
    { name: "vscode", port: 8080, path: "/" },
    { name: "api", port: 9099, path: "/", inbound: true, callers: [`user:${MEMBER_PRINCIPAL}`] },
  ],
});

function runId(): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${RUN_TASK}' LIMIT 1`);
  expect(id, `no seeded run with task ${RUN_TASK}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

test.describe("/open/{run}/{app}", () => {
  test.beforeAll(() => {
    sql(
      `SELECT audit_append(gen_random_uuid(), now(), id, 'system', 'wardynd', 'run.policy.resolve', id::text, 'success', '', '${POLICY}'::jsonb) FROM agent_runs WHERE task = '${RUN_TASK}'`,
    );
  });

  test("the owner's page mints, binds and enters, and the browser leaves for the gateway", async ({ page }) => {
    const id = runId();
    await page.goto("/healthz");
    const consoleOrigin = new URL(page.url()).origin;
    const entered = page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/__wardyn/enter" && r.request().method() === "POST",
    );
    await page.goto(`/open/${id}/api`);
    const response = await entered;
    // The gateway's own origin, never the console's.
    expect(new URL(response.url()).origin).not.toBe(consoleOrigin);
    // The ticket was accepted and spent; what stops the hand-off is the fixture, which has no sandbox (409).
    expect(response.status()).toBe(409);
  });

  test("a person the app names is told this host serves only its owner", async ({ page }) => {
    await asRealMember(page);
    await page.goto(`/open/${runId()}/api`);
    await expect(page.getByRole("alert")).toHaveText(SHARED_HOST);
  });

  test("a person nobody named, whatever their role, hears what a missing run says", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await page.goto(`/open/${runId()}/api`);
    await expect(page.getByRole("alert")).toHaveText(NOT_SHARED);
  });

  test("an app that is not inbound, and a run that does not exist, say the same", async ({ page }) => {
    await asRealMember(page);
    await page.goto(`/open/${runId()}/vscode`);
    await expect(page.getByRole("alert")).toHaveText(NOT_SHARED);
    await page.goto("/open/00000000-0000-4000-8000-000000000000/api");
    await expect(page.getByRole("alert")).toHaveText(NOT_SHARED);
  });
});
