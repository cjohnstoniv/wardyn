/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN } from "./fixtures";

// The eight secrets demos (demo-catalog.ts's "secrets" section), against the
// seeded e2e backend (real wardynd + Postgres + `-runner none`, admin-token
// auth — scripts/e2e-backend.sh). This spec drives the three that teach the
// MECHANISM (write-only / injected / approval-gated); the five per-KIND cards
// added alongside them split by walkability rather than by subject, so they are
// covered where that distinction lives: the three needsSecret ones drop from
// the walk exactly like key-never-in-the-box does below, and the two that gate
// on neither (github-app-broker, sts-fail-closed) are deep-linked in
// demos.spec.ts's KEYLESS_DEMOS walk.
//
// ACHIEVED LEVEL — UI-only, matching demos.spec.ts's own honest ceiling on
// this backend, not the full live proof the plan describes. `-runner none`
// means POST /api/v1/runs never produces a running sandbox: no terminal ever
// attaches (.xterm-screen never appears), so the mint sequence
// (409 pending -> approve -> 200 rule -> 409 already_minted), the audit
// panel's credential.mint/secret.read rows, the LiveApprovals credential
// card, and the 404 store-probe output are all UNPROVABLE here — they need a
// real runner (WARDYN_DEMO against the compose stack; see
// e2e/demo/walkthrough.spec.ts and e2e/demo/10-approvals-and-egress.spec.ts,
// which already drive `held-at-the-door`'s live decision that way). Writing
// assertions for them here would just be a spec that can only fail in CI.
//
// What IS honestly provable without a runner:
//  - write-only-by-design needs no secret and renders unconditionally.
//  - the two GRANTED demos (`needsSecret: "wardyn-demo-key"`) are dropped
//    from the walk until that secret is stored — steps.ts's stepOrder — so a
//    bare deep link to either bounces back to the nearest surviving step
//    (write-only-by-design, the section's own first demo) rather than
//    opening a step whose Start would 422. The three per-kind needsSecret
//    cards (rest-api-token, pat-stdout-only, ssh-briefly-resident) behave
//    identically, by the same one predicate — pinned deterministically in
//    steps.test.ts rather than re-driven three more times here.
//  - once the secret exists, both steps open, render their real
//    eligible_grants policy (host/header/secret_name, requires_approval),
//    and Start is disabled with the same runner-less hint every other demo
//    shows (demos.spec.ts) — never falsely offering a launch this backend
//    cannot honor.

const SECRET_NAME = "wardyn-demo-key";

async function putSecret(page: import("@playwright/test").Page, name: string, value: string) {
  const res = await page.request.put(`/api/v1/secrets/${encodeURIComponent(name)}`, {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, "Content-Type": "application/json" },
    data: { value },
  });
  expect(res.ok()).toBeTruthy();
}

async function deleteSecret(page: import("@playwright/test").Page, name: string) {
  await page.request
    .delete(`/api/v1/secrets/${encodeURIComponent(name)}`, {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
    })
    .catch(() => {});
}

test.describe("Secrets demos", () => {
  test.beforeEach(async ({ page }) => {
    // /setup renders the WELCOME hero until this flag is set (onboarding-
    // screen.tsx's GettingStarted) — pre-seed it so every `?step=` deep link
    // below lands on the demo step itself, same as demos.spec.ts.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
  });

  test.afterEach(async ({ page }) => {
    // Re-seed tolerant: never leave the demo secret behind for the next test.
    await deleteSecret(page, SECRET_NAME);
  });

  test("write-only-by-design needs no secret and proves the negative from its own step copy", async ({
    page,
  }) => {
    await page.goto("/setup?step=write-only-by-design");
    await expect(page.getByRole("heading", { name: "Write-only, even for you", level: 2 })).toBeVisible();
    await expect(page.getByTestId("demo-card-write-only-by-design")).toBeVisible();
    await expect(page.getByTestId("demos-step-not-ready")).toBeVisible();
    await expect(page.getByTestId("demo-start-write-only-by-design")).toBeDisabled();
    // The store's read-back 404 is the demo's own headline claim — pinned here
    // as printed step copy, since no sandbox can actually run the probe.
    await expect(page.getByText(/no read-back route for a stored secret/)).toBeVisible();
  });

  test("the two granted demos are dropped from the walk without their secret", async ({ page }) => {
    await page.goto("/setup?step=key-never-in-the-box");
    // stepOrder(status) excludes it (needsSecret unmet) — the re-correct
    // effect falls back to the nearest surviving step, write-only-by-design.
    await expect(
      page.getByRole("heading", { name: "Write-only, even for you", level: 2 }),
    ).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole("heading", { name: "The key that never enters the box" })).toHaveCount(0);

    await page.goto("/setup?step=authorized-not-issued");
    await expect(
      page.getByRole("heading", { name: "Write-only, even for you", level: 2 }),
    ).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole("heading", { name: "Authorized, not issued" })).toHaveCount(0);
  });

  test("storing the secret unlocks both granted demo steps, honestly gated on the runner", async ({
    page,
  }) => {
    await putSecret(page, SECRET_NAME, "e2e-demo-key-value");

    await page.goto("/setup?step=key-never-in-the-box");
    await expect(
      page.getByRole("heading", { name: "The key that never enters the box", level: 2 }),
    ).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId("demo-card-key-never-in-the-box")).toBeVisible();
    await expect(page.getByTestId("demos-step-not-ready")).toBeVisible();
    await expect(page.getByTestId("demo-start-key-never-in-the-box")).toBeDisabled();
    // The real granted policy — the header/host/secret_name this proof pins.
    const policy = page.getByTestId("demo-policy-key-never-in-the-box");
    await expect(policy).toContainText("example.com");
    await expect(policy).toContainText("wardyn-demo-key");
    await expect(policy).toContainText("api_key");

    await page.goto("/setup?step=authorized-not-issued");
    await expect(
      page.getByRole("heading", { name: "Authorized, not issued", level: 2 }),
    ).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId("demo-card-authorized-not-issued")).toBeVisible();
    await expect(page.getByTestId("demos-step-not-ready")).toBeVisible();
    await expect(page.getByTestId("demo-start-authorized-not-issued")).toBeDisabled();
    // requires_approval is what makes this demo's mint a held human decision
    // rather than an auto-grant — the load-bearing field over demo #1's policy.
    const approvalPolicy = page.getByTestId("demo-policy-authorized-not-issued");
    await expect(approvalPolicy).toContainText("requires_approval: true");
    await expect(approvalPolicy).toContainText("ttl_seconds");
  });
});
