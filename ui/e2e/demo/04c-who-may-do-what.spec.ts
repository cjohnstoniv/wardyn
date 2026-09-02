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
  await caption(page, "We sign in as the admin. Who is an admin was decided by a role map in the install's config — one line per person.");
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
  await spotlight(page, page.getByText("Egress hosts").first());
  await caption(page, "Egress hosts, for one: which destinations a member may approve on their own run.");
  await beat(page, PACE.read);
  await caption(page, "Unenforced, they can approve anything their run asks for. Enforced — only the hosts you granted them.");
  await beat(page, PACE.read);
  await beat(page, PACE.read);
  await caption(page, "And unenforced is the default.");
  await beat(page, PACE.read);
  await caption(page, "Turn them on one capability at a time, at whatever pace your organization can take.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByText("Enforcement").first());
  await caption(page, "Enforcement is the switch that turns a written rule into a refused request.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("switch", { name: /Enforcement Egress hosts/i }), "So throw it.");
  await expect(page.getByRole("alertdialog")).toBeVisible({ timeout: 15_000 });
  await caption(page, "And the product says who this changes, before it changes anything.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("alertdialog").getByRole("button", { name: "Enforce" }), "Enforced.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 3 — the same install, as a member: the boundary as a lived experience
// ---------------------------------------------------------------------------
test("V04c act 3 — the member's view of the same install", async () => {
  test.setTimeout(600_000);
  const page = stage();

  // Sign out ON CAMERA (F33): the role flip is the film.
  await page.locator("header").getByRole("button").last().click();
  await act(page, page.getByRole("button", { name: "Sign out" }).or(page.getByRole("menuitem", { name: "Sign out" })).first(), "Now the other side of the map — signing out, and back in as somebody else.");
  await page.waitForTimeout(1500);
  await page.goto("/");
  await dexSignIn("member@wardyn.local");
  await page.waitForURL(/\/(setup|runs)/, { timeout: 60_000 });

  await expect(page.getByText("You're a member of this Wardyn")).toBeVisible({ timeout: 30_000 });
  await caption(page, "A member lands on their own Getting Started.");
  await beat(page, PACE.read);
  await caption(page, "Both accounts come from the install's role map — creating people is your identity provider's job; Wardyn only reads the map.");
  await beat(page, PACE.read);
  await beat(page, PACE.read);

  await spotlight(page, page.getByRole("navigation").first());
  await caption(page, "Runs, approvals, workspaces. The console stops offering the rest.");
  await beat(page, PACE.read);
  await caption(page, "Hiding a link isn't authorization, though — the server has to refuse those calls too. You'll watch one of its refusals land on the record in 'Admin operations'.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await expect(page.getByRole("link", { name: /^Permissions/ })).toHaveCount(0);
  await expect(page.getByRole("link", { name: /^Policies/ })).toHaveCount(0);

  // The enforced capability, lived: the member's own run raises an approval
  // they are no longer allowed to grant themselves.
  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("reach for the outside world");
  await act(page, page.getByRole("radio", { name: /^Terminal/ }), "Their own run — a terminal, nothing mounted.");
  await act(page, page.getByRole("button", { name: "Minimal" }), "Minimal policy: unlisted hosts raise an approval.");
  const spec = page.getByRole("textbox", { name: "Spec (JSON)" });
  await spec.fill(JSON.stringify({
    allowed_domains: ["api.anthropic.com"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC1",
    auto_stop_after_sec: 3600,
    eligible_grants: [],
  }, null, 2));
  await caption(page, "And lowered to Fence — the one barrier this cluster has. The floor is the member's to set; the admin's ceiling still caps it.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("radio", { name: /^Fence/ }), undefined);
  await act(page, page.getByRole("button", { name: /^Launch/ }), "Launch it.");
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: 240_000 });
  await screen.click();
  await page.keyboard.type("curl -sSI --max-time 5 https://example.com\n");

  const row = page.getByTestId("live-approval-row").filter({ hasText: "example.com" }).first();
  await expect(row).toBeVisible({ timeout: 300_000 });
  await spotlight(page, row);
  await caption(page, "The request is raised. Now their own queue — where members decide what they're empowered to.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await page.goto("/approvals");
  await expect(page.getByText("Not a host you're granted").first()).toBeVisible({ timeout: 60_000 });
  await spotlight(page, page.getByText("Not a host you're granted").first());
  await caption(page, "Egress hosts is enforced, and this member holds no grant — so the console says exactly that: not a host you're granted.");
  await beat(page, PACE.read);
  await caption(page, "The decision belongs to someone the map empowered — you'll watch that hand-off in 'Admin operations'.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await caption(page, "One install. Two roles. The map decides — and the map is yours.");
  await beat(page, PACE.read);
  await caption(page, "Next on the core path: 'Your first policy'.");
  await beat(page, PACE.read);
});
