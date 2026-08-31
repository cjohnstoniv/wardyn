/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V04c — Who may do what. The multi-user path's permissions episode, filmed on
 * the cluster V02c built (SSO via deploy/kind/sso/): the People step's claim,
 * the Permissions surface behind it, and then the SAME install through a
 * member's eyes — the boundary as a lived experience, not a settings page.
 *
 *     WARDYN_DEMO_SKIP_MODEL=1 WARDYN_DEMO_BASE_URL=http://localhost:8280 \
 *       scripts/record-demo.sh --video 04c
 *
 * STATE CONTRACT. Runs AFTER V02c on the same cluster: install onboarded,
 * OIDC live (admin@wardyn.local / member@wardyn.local, both "password" —
 * deploy/kind/sso/README.md's demo literals). No sweep: this episode launches
 * nothing and the cluster is one take old.
 *
 * The sign-out/sign-in seam is the episode's own subject — the role flip IS
 * the film — so both trips through Dex are on camera.
 */

import { test, expect } from "@playwright/test";
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

async function dexSignIn(email: string): Promise<void> {
  const page = stage();
  await page.getByRole("link", { name: "Sign in with SSO" }).or(page.getByRole("button", { name: "Sign in with SSO" })).first().click();
  await page.locator('input[type="password"]').waitFor({ timeout: 30_000 });
  await page.locator('input[type="text"], input[name="login"]').first().fill(email);
  await page.locator('input[type="password"]').fill("password");
  await page.getByRole("button", { name: /log ?in/i }).click();
}

// ---------------------------------------------------------------------------
// Act 1 — the admin's map: People -> Permissions
// ---------------------------------------------------------------------------
test("V04c act 1 — the map: who signs in, and as what", async () => {
  test.setTimeout(240_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  await chapter(page, "Who may do what", "One install, two very different views of it");

  await dexSignIn("admin@wardyn.local");
  await page.waitForURL(/\/(runs|setup)/, { timeout: 60_000 });
  await caption(page, "Signed in as the admin — the role map in the chart's values decided that.");
  await beat(page, PACE.read);

  await page.goto("/setup?step=people");
  await expect(page.getByRole("heading", { name: "Who can sign in" })).toBeVisible({ timeout: 30_000 });
  await spotlight(page, page.getByText("Multi-user").first());
  await caption(page, "The People step is the claim: multi-user, role-mapped, via your identity provider.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await act(page, page.getByRole("link", { name: "Open Permissions" }), "And Permissions is where the claim becomes specific.");
  await page.waitForURL(/\/permissions/, { timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// Act 2 — the Permissions surface: capabilities, enforced one at a time
// ---------------------------------------------------------------------------
test("V04c act 2 — capabilities, enforced one at a time", async () => {
  test.setTimeout(240_000);
  const page = stage();

  await expect(page.getByRole("heading", { name: "Permissions" }).first()).toBeVisible({ timeout: 30_000 });
  await caption(page, "Each capability is granted — and enforced — on its own.");
  await beat(page, PACE.read);
  await caption(page, "Until you enforce one, nothing about it changes: adopt the model at your own pace.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByText("Enforcement").first());
  await caption(page, "Enforcement is the switch that turns a written rule into a refused request.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 3 — the same install, as a member
// ---------------------------------------------------------------------------
test("V04c act 3 — the member's view of the same install", async () => {
  test.setTimeout(300_000);
  const page = stage();

  // Sign out on camera; the role flip is the film.
  await page.context().clearCookies();
  await page.goto("/");
  await caption(page, "Now the other side of the map.");
  await dexSignIn("member@wardyn.local");
  await page.waitForURL(/\/setup/, { timeout: 60_000 });

  await expect(page.getByText("You're a member of this Wardyn")).toBeVisible({ timeout: 30_000 });
  await caption(page, "A member lands on their own Getting Started — the ceiling is set; they run inside it.");
  await beat(page, PACE.read);

  // The nav is the boundary made visible: no Policies, no Permissions, no Secrets.
  await spotlight(page, page.getByRole("navigation").first());
  await caption(page, "Runs, approvals, workspaces. The configuring surfaces aren't hidden — they're not theirs.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await expect(page.getByRole("link", { name: /^Permissions/ })).toHaveCount(0);
  await expect(page.getByRole("link", { name: /^Policies/ })).toHaveCount(0);

  // Typing the URL is not a workaround.
  await page.goto("/permissions");
  await expect(page.getByRole("heading", { name: "Permissions" })).toHaveCount(0, { timeout: 15_000 });
  await caption(page, "And typing the address is not a workaround — refused, without pretending the page never existed less honestly than that.");
  await beat(page, PACE.read);

  await caption(page, "One install. Two roles. The map decides — and the map is yours.");
  await beat(page, PACE.read);
});
