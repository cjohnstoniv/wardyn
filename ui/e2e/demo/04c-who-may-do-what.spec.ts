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

import { test, expect, type Page } from "@playwright/test";
import { NR_DOCUMENT } from "../../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../../src/app/components/wardyn/copy/new-run/review";
import { NR_SHARED } from "../../src/app/components/wardyn/copy/new-run/shared";
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
import {
  confirmLaunchButton,
  NEW_RUN_HEADING,
  newRunHeading,
  nextStep,
  noExtraFiles,
  openNameDialog,
  runTitleField,
  shapeCard,
  startGuided,
  viewRunButton,
} from "./new-run";
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

/**
 * The member's run policy, written where the guided flow keeps it. Review's
 * JSON view opens the run document; its inline_policy is the policy, so the
 * rules are merged into the document the draft already is.
 *
 * OPEN: the Minimal preset and the spec box (the old Policy panel) have no
 * step in the guided flow. The policy is filmed through Review's JSON view and
 * the document screen; the lead decides whether that is the home.
 */
async function setInlinePolicy(page: Page, policy: Record<string, unknown>, text?: string): Promise<void> {
  const viewAs = page.getByLabel(NR_REVIEW.VIEW_ARIA).getByRole("button", { name: NR_REVIEW.VIEW_JSON });
  await act(page, viewAs, text);
  await act(page, page.getByRole("button", { name: NR_REVIEW.EDIT_FORMAT("JSON") }));
  const editor = page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("JSON") });
  await expect(editor).toBeVisible();
  const document = JSON.parse(await editor.inputValue());
  document.intent.inline_policy = policy;
  await editor.fill(JSON.stringify(document, null, 2));
  await act(page, page.getByRole("button", { name: NR_DOCUMENT.CONTINUE_REVIEW }));
  await expect(newRunHeading(page, NEW_RUN_HEADING.review)).toBeVisible();
}

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

  await page.goto("/admin/setup?step=people");
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
  // they are no longer allowed to grant themselves. The guided flow sets the
  // order: the barrier on Run, the policy on Review, the title in the Name dialog.
  await page.goto("/runs/new");
  await expect(newRunHeading(page, NEW_RUN_HEADING.start)).toBeVisible({ timeout: 30_000 });
  await startGuided(page);
  await act(page, shapeCard(page, "environment"), "Their own run — a terminal, nothing mounted.");
  await nextStep(page, NEW_RUN_HEADING.runEnvironment);
  await caption(page, "And lowered to Fence — the one barrier this cluster has. The floor is the member's to set; the admin's ceiling still caps it.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("radio", { name: new RegExp(`^${NR_SHARED.BARRIER_FENCE}`) }), undefined);
  await nextStep(page, NEW_RUN_HEADING.harnesses);
  await nextStep(page, NEW_RUN_HEADING.files);
  await act(page, noExtraFiles(page));
  await nextStep(page, NEW_RUN_HEADING.tools);
  await nextStep(page, NEW_RUN_HEADING.review);
  // The floor stays in the policy too: the one-line edit is the lesson.
  await setInlinePolicy(
    page,
    {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC1",
      auto_stop_after_sec: 3600,
      eligible_grants: [],
    },
    "The run's policy: unlisted hosts raise an approval.",
  );
  await openNameDialog(page, "Launch it.");
  await runTitleField(page).fill("reach for the outside world");
  await act(page, confirmLaunchButton(page));
  await viewRunButton(page).click({ timeout: 240_000 });
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
