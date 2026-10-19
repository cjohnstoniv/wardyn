/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  test,
  expect,
  ADMIN_TOKEN,
  MEMBER_PRINCIPAL,
  asRealMember,
  chooseShape,
  gotoConsole,
  mockMemberRole,
  navToRoute,
  sidebarLink,
  startGuided,
} from "./fixtures";
import { GOVERNANCE as GOV, MEMBER } from "../src/app/lib/governance-copy";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import type { Page } from "@playwright/test";

// Split out of governance.spec.ts (#209): the member's own view of a
// governance ceiling. Every case here is route-spliced or role-mocked, never
// dependent on the real "walled" profile governance.spec.ts's authoring walk
// writes to Postgres — see the mockAssignedCeiling comment below (around
// line 39) for why the field these splices patch can never arrive from a
// real request on this harness.
test.describe.configure({ mode: "serial" });

const NAME = "walled";
// The pool list every hermetic page gets (fixtures.ts): the backend runs no runner and has no pool of its own.
const POOLS = SCENARIO_POOLS.member;

// New Run's Run step, as a person reaches it: Start, guided setup, an Environment.
async function openRunStep(page: Page): Promise<void> {
  await navToRoute(page, "/runs/new");
  await expect(page.getByRole("heading", { name: NR_START.TITLE })).toBeVisible();
  await startGuided(page);
  await chooseShape(page, "environment");
}

test.describe("governance — a member sees none of it (mocked /me role)", () => {
  test("Governance is absent from the member nav, beside the rest of the admin set", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    // /governance is deliberately not in the member nav — there is no member governance
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
// The member's view of a ceiling. The splice cases below patch
// governance_profile_name onto the REAL /policies/default response —
// route.fetch() + patch + refulfill, the same technique fixtures.ts's
// mockMemberRole documents, so the shape around it stays genuine. The mocked
// role keeps the operator bearer, and effectiveCeiling short-circuits for an
// operator, so the field cannot arrive that way; the real-member case further
// down assigns a profile to asRealMember's principal and reads it back through
// the member's own bearer. The resolver's own answer is pinned server-side
// (governance_ceiling_test.go:432).
// ---------------------------------------------------------------------------
async function mockAssignedCeiling(page: Page, name: string): Promise<void> {
  await page.route("**/api/v1/policies/default", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.governance_profile_name = name;
    await route.fulfill({ response, json });
  });
  // The Run step reads the same profile off the pool list's allowance, so it is spliced there too.
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.request().method() !== "GET"
      ? route.fallback()
      : route.fulfill({ json: { ...POOLS, allowance: { active: 0, ...POOLS.allowance, profile_name: name } } }),
  );
}

test.describe("governance — the member is told which ceiling bounds them", () => {
  test("the Run step names the assigned profile in the per-run limits note", async ({ page }) => {
    await mockMemberRole(page);
    await mockAssignedCeiling(page, NAME);
    await gotoConsole(page);
    await openRunStep(page);

    // The limits note names whose ceiling bounds the run, beside the pool's own and the server's.
    await expect(page.getByText(NR_RUN.LIMITS_NOTE(NAME))).toBeVisible();
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

    await openRunStep(page);
    await expect(page.getByText(/the governance profile your admin assigned you/)).toHaveCount(0);
    await expect(page.getByText(NR_RUN.LIMITS_NOTE(NAME))).toHaveCount(0);
  });
});

// The assignment is real: a profile written through the admin API and bound to the member's own
// principal, then read back through that member's bearer (no splice), so the resolver, the
// redaction and the screen are all the production ones.
test.describe("governance — a real member is told which ceiling bounds them", () => {
  const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
  const REAL_NAME = "real-member-wall";

  test("a profile assigned to the member's principal reaches the member's own Getting Started card", async ({ page }) => {
    const prof = await page.request.post("/api/v1/governance/profiles", {
      headers: auth,
      data: {
        name: REAL_NAME,
        ceiling: {
          allowed_domains: ["api.anthropic.com"],
          denied_domains: [],
          first_use_approval: "deny_with_review",
          min_confinement_class: "CC3",
          auto_stop_after_sec: 3600,
          eligible_grants: [],
          tool_rules: [{ tool: "*", effect: "allow" }],
        },
        limits: {},
      },
    });
    expect(prof.status(), await prof.text()).toBe(201);
    const profileID = (await prof.json()).profile.id;
    try {
      const assigned = await page.request.post("/api/v1/governance/assignments", {
        headers: auth,
        data: { subject_type: "user", subject: MEMBER_PRINCIPAL, profile_id: profileID, priority: 0 },
      });
      expect(assigned.ok(), await assigned.text()).toBeTruthy();

      await asRealMember(page);
      await gotoConsole(page);
      await navToRoute(page, "/setup");
      await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
      await expect(page.getByText(MEMBER.GS_CHIP(REAL_NAME))).toBeVisible();
      await expect(page.getByText(MEMBER.GS_BODY(REAL_NAME))).toBeVisible();
    } finally {
      const snap = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
      for (const a of snap.assignments ?? []) {
        if (a.profile_id === profileID) await page.request.delete(`/api/v1/governance/assignments/${a.id}`, { headers: auth });
      }
      await page.request.delete(`/api/v1/governance/profiles/${profileID}`, { headers: auth });
    }
  });
});

// ---------------------------------------------------------------------------
// #1200 — the barrier choice on the member's own New Run page: which of
// Fence / Wall / Vault the Run step offers, the decided state it collapses to
// with exactly one choice left, and T-9's requirement when the floor and the
// host disagree. The Run step reads its barriers from the pool list
// (GET /runner-pools, `barrier_options`: disabled plus a reason, the default
// and its source), so that list is what is spliced. The hermetic runner
// (`-runner none`) installs no tier at all, and the floor is an admin's
// setting a member session cannot mint, so a real answer cannot be asked for.
// ---------------------------------------------------------------------------

interface BarrierSplice {
  class: "CC1" | "CC2" | "CC3";
  available: boolean;
  default?: boolean;
  default_source?: "organisation" | "server";
  reason_code?: "below_floor" | "not_offered";
  reason_detail?: "kvm_missing";
  floor?: "CC2" | "CC3";
}

async function mockBarriers(page: Page, options: BarrierSplice[]): Promise<void> {
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.request().method() !== "GET"
      ? route.fallback()
      : route.fulfill({ json: { ...POOLS, pools: POOLS.pools.map((pool) => ({ ...pool, barrier_options: options })) } }),
  );
}

const barrier = (page: Page, name: string) => page.getByRole("radio", { name: new RegExp(`^${name}`) });

const BELOW_VAULT: BarrierSplice[] = [
  { class: "CC1", available: false, reason_code: "below_floor", floor: "CC3" },
  { class: "CC2", available: false, reason_code: "below_floor", floor: "CC3" },
];

test.describe("governance — the member's own barrier choice obeys the floor (T-9)", () => {
  test("a Vault floor, with Vault available, leaves one choice — Vault, set by the organisation", async ({ page }) => {
    await mockMemberRole(page);
    await mockBarriers(page, [...BELOW_VAULT, { class: "CC3", available: true, default: true, default_source: "organisation" }]);
    await gotoConsole(page);
    await openRunStep(page);

    // Fence and Wall stay on the table, shut, each saying why; Vault is the only live choice.
    await expect(page.getByText(NR_RUN.BARRIER_REQUIRED(NR_SHARED.BARRIER_VAULT)).first()).toBeVisible();
    await expect(barrier(page, NR_SHARED.BARRIER_FENCE)).toBeDisabled();
    await expect(barrier(page, NR_SHARED.BARRIER_WALL)).toBeDisabled();
    await expect(barrier(page, NR_SHARED.BARRIER_VAULT)).toBeEnabled();
    await expect(barrier(page, NR_SHARED.BARRIER_VAULT)).toBeChecked();
    await expect(page.getByText(NR_RUN.BADGE_ORG_DEFAULT).first()).toBeVisible();
  });

  test("a Vault floor the host cannot build shows the requirement, never a silent fallback", async ({ page }) => {
    await mockMemberRole(page);
    // This host only has Fence/Wall — the floor and the host disagree.
    await mockBarriers(page, [
      ...BELOW_VAULT,
      { class: "CC3", available: false, reason_code: "not_offered", reason_detail: "kvm_missing" },
    ]);
    await gotoConsole(page);
    await openRunStep(page);

    await expect(page.getByText(NR_RUN.BARRIER_REQUIRED(NR_SHARED.BARRIER_VAULT)).first()).toBeVisible();
    await expect(page.getByText(NR_RUN.BARRIER_NO_KVM).first()).toBeVisible();
    // Never the false claim that Wall (the strongest tier this host DOES have) is what the member gets.
    await expect(barrier(page, NR_SHARED.BARRIER_WALL)).toBeDisabled();
    await expect(barrier(page, NR_SHARED.BARRIER_WALL)).not.toBeChecked();
    await expect(barrier(page, NR_SHARED.BARRIER_VAULT)).toBeDisabled();
  });

  test("a tier the host hasn't installed is never selectable on the member's page, floor or not", async ({ page }) => {
    await mockMemberRole(page);
    await mockBarriers(page, [
      { class: "CC1", available: true },
      { class: "CC2", available: true, default: true, default_source: "server" },
      { class: "CC3", available: false, reason_code: "not_offered" },
    ]);
    await gotoConsole(page);
    await openRunStep(page);

    await expect(barrier(page, NR_SHARED.BARRIER_FENCE)).toBeEnabled();
    await expect(barrier(page, NR_SHARED.BARRIER_WALL)).toBeEnabled();
    await expect(barrier(page, NR_SHARED.BARRIER_VAULT)).toBeDisabled();
    await expect(page.getByText(NR_RUN.BARRIER_NONE_OFFERS(NR_SHARED.BARRIER_VAULT)).first()).toBeVisible();
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
