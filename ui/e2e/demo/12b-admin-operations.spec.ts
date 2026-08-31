/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V12b — Admin operations: four eyes on egress. The multi-user path's
 * governance-of-the-governors episode, filmed on the V02c/V04c cluster. One
 * subject, told twice: with WARDYN_EGRESS_SECOND_HUMAN set, the human who
 * created a run may NOT decide its egress approvals — the member watches
 * their own Approve refused with the switch's name in the message, and the
 * admin (a second human) is who actually decides. The audit trail carries
 * both halves: authz.denied (second_human_required) and the admin's
 * approval.decide.
 *
 *     WARDYN_DEMO_SKIP_MODEL=1 WARDYN_DEMO_BASE_URL=http://localhost:8280 \
 *       scripts/record-demo.sh --video 12b
 *
 * STATE CONTRACT (staged off camera; narration names it honestly):
 *   - The V02c cluster, onboarded, SSO live (admin@/member@wardyn.local),
 *     Egress hosts ENFORCED (V04c throws that switch on camera and leaves it
 *     on — which is why act 1 must GRANT the member the host before four-eyes
 *     has anything left to say).
 *   - Four-eyes ON before the take:
 *       helm --kube-context kind-wardyn-quickstart upgrade wardyn \
 *         deploy/helm/wardyn -n wardyn --reuse-values \
 *         --set env.WARDYN_EGRESS_SECOND_HUMAN=true \
 *       && kubectl --context kind-wardyn-quickstart -n wardyn rollout status deploy/wardyn
 *   - No terminal lane: the flip is chart configuration, not film — the
 *     episode's one spoken claim about it is that it is one line, default off.
 *
 * The rest of the admin-operations material (key management, token rotation,
 * support bundles) deliberately stays with episode 12's fleet story — this
 * episode is one subject told well, per the persona rounds' standing bill
 * against two-subject episodes.
 */

import { test, expect } from "@playwright/test";
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

const HOST = "example.com";

async function dexSignIn(email: string): Promise<void> {
  const page = stage();
  await page.getByRole("link", { name: "Sign in with SSO" }).or(page.getByRole("button", { name: "Sign in with SSO" })).first().click();
  await page.locator('input[type="password"]').waitFor({ timeout: 30_000 });
  await page.locator('input[type="text"], input[name="login"]').first().fill(email);
  await page.locator('input[type="password"]').fill("password");
  await page.getByRole("button", { name: /log ?in/i }).click();
}

// ---------------------------------------------------------------------------
// Act 1 — the admin grants the host (so four-eyes is the ONLY bar left)
// ---------------------------------------------------------------------------
test("V12b act 1 — grant them the host", async () => {
  test.setTimeout(300_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  await chapter(page, "Admin operations", "Four eyes on egress");

  await dexSignIn("admin@wardyn.local");
  await page.waitForURL(/\/(runs|setup)/, { timeout: 60_000 });
  await page.goto("/permissions");
  await expect(page.getByRole("heading", { name: "Permissions" }).first()).toBeVisible({ timeout: 30_000 });
  await caption(page, "Egress hosts is enforced — so before anything else, grant the member the host their work needs.");
  await beat(page, PACE.read);
  await page.getByRole("textbox", { name: "Who" }).fill("member@wardyn.local");
  await page.getByRole("combobox", { name: "Capability" }).selectOption({ label: "Egress hosts" }).catch(async () => {
    // Not a native select — drive it as a listbox.
    await page.getByRole("combobox", { name: "Capability" }).click();
    await page.getByRole("option", { name: "Egress hosts" }).click();
  });
  await page.getByRole("textbox", { name: "Host" }).fill("example.com");
  await act(page, page.getByRole("button", { name: "Add grant" }).first(), "Granted: this member may decide example.com on their own runs. Almost.");
  // The row is the proof the grant landed — without this a silently failed
  // add turns act 3's four-eyes refusal into an ungranted-host chip instead.
  await expect(
    page.getByText("member@wardyn.local").first(),
  ).toBeVisible({ timeout: 15_000 });
  await beat(page, PACE.read);
  await caption(page, "Because this deployment also opted into four-eyes: one line in the chart, default off.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 2 — the member's run raises an approval
// ---------------------------------------------------------------------------
test("V12b act 2 — a member's run asks for the outside world", async () => {
  test.setTimeout(420_000);
  const page = stage();

  await page.locator("header").getByRole("button").last().click();
  await page.getByRole("button", { name: "Sign out" }).or(page.getByRole("menuitem", { name: "Sign out" })).first().click();
  await page.waitForTimeout(1500);
  await page.goto("/");
  await dexSignIn("member@wardyn.local");
  await page.waitForURL(/\/(runs|setup)/, { timeout: 60_000 });

  await page.goto("/runs/new");
  await expect(page.getByRole("heading", { name: "New run", level: 1 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "A member launches a run of their own: a terminal, nothing mounted, Minimal policy.");
  await beat(page, PACE.read);
  await page.getByRole("combobox", { name: "Title" }).fill("reach for the outside world");
  await act(page, page.getByRole("radio", { name: /^Terminal/ }), "No agent needed — a shell is enough to meet the boundary.");
  await act(page, page.getByRole("button", { name: "Minimal" }), "Minimal policy: unlisted hosts raise an approval —");
  // Minimal ships a CC2 floor; this demo cluster's one barrier is the Fence.
  // The one-line edit is itself the lesson: the floor is the member's to
  // RAISE, never to sneak under the admin's ceiling.
  const spec = page.getByRole("textbox", { name: "Spec (JSON)" });
  await spec.fill(JSON.stringify({
    allowed_domains: ["api.anthropic.com"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC1",
    auto_stop_after_sec: 3600,
    eligible_grants: [],
  }, null, 2));
  await caption(page, "— floored to this cluster's Fence.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("radio", { name: /^Fence/ }), undefined);
  await act(page, page.getByRole("button", { name: /^Launch/ }), "Launch it interactive.");

  // The cockpit: terminal + inline approvals. Ask for a host the policy
  // doesn't list.
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: 240_000 });
  await screen.click();
  await page.keyboard.type(`curl -sSI --max-time 5 https://${HOST}\n`);
  await caption(page, "Denied — and raised for review, by name.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 2 — the member may not decide their own run's egress
// ---------------------------------------------------------------------------
test("V12b act 3 — your run, therefore not your call", async () => {
  test.setTimeout(240_000);
  const page = stage();

  const raised = page.getByTestId("live-approval-row").filter({ hasText: HOST }).first();
  await expect(raised).toBeVisible({ timeout: 300_000 });
  await caption(page, "Raised. Off to the member's own queue — they hold the grant for this very host.");
  await beat(page, PACE.read);
  await page.goto("/approvals");
  const row = page.getByRole("button", { name: "Approve" }).first();
  await expect(row).toBeVisible({ timeout: 60_000 });
  await caption(page, "Granted host, their own run — every rule so far says yes.");
  await beat(page, PACE.read);
  await row.click();
  // The page confirms before deciding — the confirm is where the server answers.
  const confirm = page.getByRole("dialog").getByRole("button", { name: /approve/i }).first();
  await confirm.waitFor({ timeout: 10_000 });
  await confirm.click();

  // The 403's own words, surfaced as the page's error toast.
  await expect(page.getByText(/second human must decide/i).first()).toBeVisible({ timeout: 15_000 });
  await spotlight(page, page.getByText(/second human must decide/i).first());
  await caption(page, "Refused — you created this run, so someone else approves or denies its egress.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "Not a permissions gap. A guarantee: no one self-approves their own agent's reach.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 3 — the second human decides
// ---------------------------------------------------------------------------
test("V12b act 4 — the second human", async () => {
  test.setTimeout(300_000);
  const page = stage();

  await page.context().clearCookies();
  await page.goto("/");
  await dexSignIn("admin@wardyn.local");
  await page.waitForURL(/\/(runs|setup)/, { timeout: 60_000 });
  await caption(page, "A second human — the admin — with their own queue.");
  await beat(page, PACE.read);
  await page.goto("/approvals");
  await caption(page, "Same row, different human: this decision is allowed to exist.");
  await beat(page, PACE.read);
  await page.getByRole("button", { name: "Approve" }).first().click();
  const confirm = page.getByRole("dialog").getByRole("button", { name: /approve/i }).first();
  await confirm.waitFor({ timeout: 10_000 });
  await confirm.click();
  await expect(page.getByText(/request approved/i).first()).toBeVisible({ timeout: 15_000 });
  await caption(page, "Approved — by someone who did not create the run.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 4 — both halves on the record
// ---------------------------------------------------------------------------
test("V12b act 5 — the trail holds both halves", async () => {
  test.setTimeout(240_000);
  const page = stage();

  await page.goto("/audit");
  const search = page.getByPlaceholder("Search events, domains, run IDs…");
  await search.fill("authz.denied");
  await expect(page.getByText("authz.denied").first()).toBeVisible({ timeout: 60_000 });
  await spotlight(page, page.getByText("authz.denied").first());
  await caption(page, "The refusal is a row: authz.denied, reason second-human-required.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await search.fill("approval.decide");
  await expect(page.getByText(/Decided an approval request/).first()).toBeVisible({ timeout: 30_000 });
  await spotlight(page, page.getByText(/Decided an approval request/).first());
  await caption(page, "And the decision is a row with the second human's name on it.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "Four eyes, one switch, receipts for both.");
  await beat(page, PACE.read);
});
