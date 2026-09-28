/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, mockMemberRole, navToRoute, sidebarLink } from "./fixtures";
import { GOVERNANCE as GOV, MEMBER } from "../src/app/lib/governance-copy";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";
import type { Page } from "@playwright/test";

// Split out of governance.spec.ts (#209): the member's own view of a
// governance ceiling. Every case here is route-spliced or role-mocked, never
// dependent on the real "walled" profile governance.spec.ts's authoring walk
// writes to Postgres — see this file's own header comment for why the field
// these splices patch can never arrive from a real request on this harness.
test.describe.configure({ mode: "serial" });

const NAME = "walled";

test.describe("governance — a member sees none of it (mocked /me role)", () => {
  test("Governance is absent from the member nav, beside the rest of the admin set", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    // MEMBER_NAV_PATHS (app-shell.tsx) is the single source of truth, and
    // /governance is deliberately not in it — there is no member governance
    // route at all, and every route behind it is securityOps server-side.
    await expect(sidebarLink(page, "Governance")).toHaveCount(0);
    for (const label of ["Policies", "Permissions", "Secrets", "Audit", "Recordings"] as const) {
      await expect(sidebarLink(page, label)).toHaveCount(0);
    }
    for (const label of ["Runs", "Approvals", "Workspaces"] as const) {
      await expect(sidebarLink(page, label)).toBeVisible();
    }
  });
});

// ---------------------------------------------------------------------------
// The member's view of a ceiling. Splices governance_profile_name onto the
// REAL /policies/default response — route.fetch() + patch + refulfill, the
// same technique fixtures.ts's mockMemberRole documents, so the shape around
// it stays genuine. The field itself can never arrive here: effectiveCeiling
// short-circuits for an operator and this harness has no non-operator
// principal. The resolver's own answer is pinned server-side
// (governance_ceiling_test.go:432).
// ---------------------------------------------------------------------------
async function mockAssignedCeiling(page: Page, name: string): Promise<void> {
  await page.route("**/api/v1/policies/default", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.governance_profile_name = name;
    await route.fulfill({ response, json });
  });
}

test.describe("governance — the member is told which ceiling bounds them", () => {
  test("the run rail names the assigned profile, above the policy it clamps", async ({ page }) => {
    await mockMemberRole(page);
    await mockAssignedCeiling(page, NAME);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await expect(page.getByText(MEMBER.CEILING_PROFILE(NAME))).toBeVisible();
    // Under the rail's Ceiling section — first, because it bounds everything
    // below it.
    await expect(page.getByText(GOV.CEILING_TITLE, { exact: true })).toBeVisible();
  });

  test("the member Getting Started card names it too — the chip and the line", async ({ page }) => {
    await mockMemberRole(page);
    await mockAssignedCeiling(page, NAME);
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText(MEMBER.GS_CHIP(NAME))).toBeVisible();
    await expect(page.getByText(MEMBER.GS_BODY(NAME))).toBeVisible();
  });

  test("with NO assignment there is no chip, no line and no placeholder", async ({ page }) => {
    // Unspliced: the absent-row doctrine. This is the arm that needs no mock,
    // because the real backend genuinely omits the key for this caller.
    await mockMemberRole(page);
    await gotoConsole(page);

    await navToRoute(page, "/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText(MEMBER.GS_CHIP(NAME))).toHaveCount(0);
    await expect(page.getByText(/Governance ·/)).toHaveCount(0);
    await expect(page.getByText(/Your runs are bounded by/)).toHaveCount(0);

    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await expect(page.getByText(/the governance profile your admin assigned you/)).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// #1200 — the shared TierPicker on the member's own New Run page: the
// installed ∧ allowed filter, the decided state it collapses to with exactly
// one tier left, and T-9's requirement card when the floor and the host
// disagree. mockAssignedCeiling above only ever named a PROFILE; this splices
// the floor itself (min_confinement_class) the same documented way, plus
// /setup/status's confinement_classes — this harness's own runner (`-runner
// none`) advertises none at all, so "what this host has installed" has to be
// spliced too, or every tier would read as unknown rather than as a real
// install fact.
// ---------------------------------------------------------------------------

async function mockGovernanceFloor(page: Page, floor: string, profileName: string): Promise<void> {
  await page.route("**/api/v1/policies/default", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.min_confinement_class = floor;
    json.governance_profile_name = profileName;
    await route.fulfill({ response, json });
  });
}

async function mockInstalledTiers(page: Page, classes: string[]): Promise<void> {
  await page.route("**/api/v1/setup/status", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.runner = { ...json.runner, driver: "docker", confinement_classes: classes };
    await route.fulfill({ response, json });
  });
}

test.describe("governance — the member's own picker obeys the floor (T-9)", () => {
  test("a Vault floor, with Vault installed, leaves no picker at all — 'Vault' decided", async ({ page }) => {
    await mockMemberRole(page);
    await mockGovernanceFloor(page, "CC3", "vault-required");
    await mockInstalledTiers(page, ["CC1", "CC2", "CC3"]);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await expect(page.getByText("Vault · set by your admin")).toBeVisible();
    // No control at all — not Fence/Wall disabled, not present. (The
    // authored-policy spec's OWN floor chip, unrelated to this governance
    // floor, legitimately renders "Fence" elsewhere on this page — the
    // radiogroup is what actually proves "no picker".)
    await expect(page.getByRole("radio", { name: "Fence" })).toHaveCount(0);
    await expect(page.getByRole("radio", { name: "Wall" })).toHaveCount(0);
    await expect(page.getByRole("radio", { name: "Vault" })).toHaveCount(0);
  });

  test("a Vault floor the host cannot build shows the requirement, never a silent fallback", async ({ page }) => {
    await mockMemberRole(page);
    await mockGovernanceFloor(page, "CC3", "vault-required");
    // This host only has Fence/Wall — the floor and the host disagree.
    await mockInstalledTiers(page, ["CC1", "CC2"]);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    // Review P2-6: the governance-sourced wording, not the generic one —
    // this member's floor IS the governance ceiling's doing.
    await expect(
      page.getByText(/Your admin requires Vault, and this host can't run it/),
    ).toBeVisible();
    // Never the false claim that Wall (the strongest tier this host DOES
    // have) is what the member gets.
    await expect(page.getByText("Wall · set by your admin")).toHaveCount(0);
    await expect(page.getByRole("radio", { name: "Wall" })).toHaveCount(0);
  });

  test("a tier the host hasn't installed never shows on the member's page, floor or not", async ({ page }) => {
    await mockMemberRole(page);
    await mockInstalledTiers(page, ["CC1", "CC2"]); // no Vault, no floor spliced
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await expect(page.getByRole("radio", { name: "Fence" })).toBeVisible();
    await expect(page.getByRole("radio", { name: "Wall" })).toBeVisible();
    await expect(page.getByRole("radio", { name: "Vault" })).toHaveCount(0);
  });
});

// X3-F5 — /admin/governance is /admin/permissions' securityOps sibling:
// hidden from a member's nav, reachable by typing the URL. M-1b: Governance
// moved under /admin/*, so a member's own 403/500 render (what this test used
// to pin) is now unreachable — the admin-view gate refuses them, and nothing
// is fetched at all (admin-member-modes-design.md §2.3's refusal-page row).
test.describe("Governance — a member by URL is told the tier, not an outage", () => {
  test("the admin-view refusal, before /api/v1/governance is ever asked", async ({ page }) => {
    await mockMemberRole(page);
    let fetched = false;
    await page.route("**/api/v1/governance", async (route) => {
      fetched = true;
      await route.fallback();
    });
    await gotoConsole(page);
    await navToRoute(page, "/admin/governance");

    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
    await expect(page.getByText(VIEW_REFUSAL.BODY)).toBeVisible();
    await expect(page.getByRole("button", { name: GOV.FETCH_FAILED_TITLE })).toHaveCount(0);
    expect(fetched).toBe(false);
  });
});
