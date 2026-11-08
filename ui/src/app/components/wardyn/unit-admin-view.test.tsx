/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A role `user` who holds a unit-admin grant reaches Governance and Audit. Every default here fails closed:
// no /me, a failed /me, or a /me without `unit_admin_of` all mean no access.
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("../../lib/api/runs", () => ({ runs: { gradePolicy: () => Promise.resolve({ overall_risk: "medium", risk_assessment: [] }) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getGovernanceMock = vi.fn();
vi.mock("../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/governance")>("../../lib/api/governance");
  return {
    ...actual,
    governance: {
      getGovernance: () => getGovernanceMock(),
      previewGovernance: () => Promise.resolve({}),
      listChanges: () => Promise.resolve([]),
    },
  };
});
vi.mock("../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/directory")>("../../lib/api/directory");
  return { ...actual, directory: { search: () => Promise.resolve(null) } };
});
vi.mock("../../lib/api/user-types", () => ({ userTypes: { listUserTypes: () => Promise.resolve([]) } }));

import { AppShell } from "../screens/app-shell";
import { GovernanceScreen } from "../screens/governance/governance-screen";
import { OperatorProvider, RoleProvider, useUnitAdminOf } from "./operator-context";
import { ThemeProvider } from "./theme-provider";
import { ViewAccessProvider, ViewGate } from "./console-view";
import { VIEW_REFUSAL } from "./copy/console-view";
import { GOVERNANCE_NAV_TITLE } from "../../lib/nav-copy";
import { GOVERNANCE as GOV } from "../../lib/governance-copy";
import type { GovernanceSnapshot } from "../../lib/api/governance";
import { aheadByHours } from "../../lib/test-clock";

const UNITS = [{ unit_id: "u1", unit_name: "payments" }];

afterEach(() => vi.unstubAllGlobals());

function Probe() {
  return <span data-testid="units">{JSON.stringify(useUnitAdminOf())}</span>;
}

describe("useUnitAdminOf", () => {
  it("is [] with no provider and with a provider that passes none", () => {
    const { unmount } = render(<Probe />);
    expect(screen.getByTestId("units")).toHaveTextContent("[]");
    unmount();
    render(
      <OperatorProvider operator>
        <Probe />
      </OperatorProvider>,
    );
    expect(screen.getByTestId("units")).toHaveTextContent("[]");
  });

  it("is the list the provider was given", () => {
    render(
      <OperatorProvider operator={false} unitAdminOf={UNITS}>
        <Probe />
      </OperatorProvider>,
    );
    expect(screen.getByTestId("units")).toHaveTextContent(JSON.stringify(UNITS));
  });
});

describe("ViewGate — a user with a unit-admin grant", () => {
  function gateAt(path: string, units?: typeof UNITS) {
    render(
      <MemoryRouter initialEntries={[path]}>
        <OperatorProvider operator={false} unitAdminOf={units}>
          <RoleProvider role="user" roleResolved>
            <ViewAccessProvider value="user-only">
              <Routes>
                <Route element={<ViewGate fallback={null} />}>
                  <Route path="*" element={<span data-testid="outlet" />} />
                </Route>
              </Routes>
            </ViewAccessProvider>
          </RoleProvider>
        </OperatorProvider>
      </MemoryRouter>,
    );
  }

  it.each(["/admin/governance", "/admin/audit"])("%s: the outlet with a unit, the refusal without", (path) => {
    gateAt(path, UNITS);
    expect(screen.getByTestId("outlet")).toBeInTheDocument();
    expect(screen.queryByText(VIEW_REFUSAL.TITLE)).toBeNull();
  });

  it.each(["/admin/governance", "/admin/audit"])("%s without a unit is refused", (path) => {
    gateAt(path);
    expect(screen.getByText(VIEW_REFUSAL.TITLE)).toBeInTheDocument();
    expect(screen.queryByTestId("outlet")).toBeNull();
  });

  it.each(["/admin/runs", "/admin/people", "/admin/audit/x"])("%s is refused even with a unit", (path) => {
    gateAt(path, UNITS);
    expect(screen.getByText(VIEW_REFUSAL.TITLE)).toBeInTheDocument();
    expect(screen.queryByTestId("outlet")).toBeNull();
  });
});

describe("the User view's nav", () => {
  function renderShell(me: Record<string, unknown> | "fail") {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz")) {
          return Promise.resolve({ ok: true, json: async () => ({ trust_domain: "wardyn.local", identity_provider: "dex" }) });
        }
        if (u.endsWith("/api/v1/me")) {
          return me === "fail"
            ? Promise.resolve({ ok: false, status: 500, json: async () => ({}) })
            : Promise.resolve({ ok: true, json: async () => me });
        }
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    render(
      <MemoryRouter>
        <ThemeProvider>
          <Routes>
            <Route element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
              <Route index element={<span data-testid="page" />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
  }
  const MEMBER = { principal: "tok-u", method: "token", role: "user", operator: false, security_operator: false };
  const link = (name: string) => screen.queryAllByRole("link", { name: new RegExp(`^${name}`) });

  it("a unit admin sees Governance, then Audit, under Limits", async () => {
    renderShell({ ...MEMBER, unit_admin_of: UNITS });
    await waitFor(() => expect(link(GOVERNANCE_NAV_TITLE)).not.toHaveLength(0));
    const nav = screen.getAllByRole("navigation")[0];
    const labels = within(nav)
      .getAllByRole("link")
      .map((el) => el.textContent?.replace(/\d+$/, ""));
    expect(labels.slice(-2)).toEqual([GOVERNANCE_NAV_TITLE, "Audit"]);
    expect(link(GOVERNANCE_NAV_TITLE)[0]).toHaveAttribute("href", "/admin/governance");
    expect(link("Audit")[0]).toHaveAttribute("href", "/admin/audit");
    const heading = within(nav).getByTestId("nav-group-limits");
    expect(heading).toHaveTextContent("Limits");
    expect(heading.nextElementSibling).toBe(link(GOVERNANCE_NAV_TITLE)[0]);
  });

  it.each([
    ["an empty grant list", { ...MEMBER, unit_admin_of: [] }],
    ["a /me without the key", MEMBER],
    ["a failed /me", "fail" as const],
  ])("%s: neither link", async (_name, me) => {
    renderShell(me);
    // Wait for /me to have settled: a member's own nav is there, or the failure banner is.
    await waitFor(() => expect(screen.queryByTestId("page")).toBeInTheDocument());
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));
    expect(link(GOVERNANCE_NAV_TITLE)).toHaveLength(0);
    expect(link("Audit")).toHaveLength(0);
  });
});

describe("GovernanceScreen — a unit admin", () => {
  const PROFILE = {
    id: "p1",
    name: "Payments",
    ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
    limits: {},
    created_at: aheadByHours(-1),
    updated_at: aheadByHours(-1),
  };
  const SNAPSHOT = { profiles: [PROFILE], assignments: [] } as unknown as GovernanceSnapshot;

  beforeEach(() => getGovernanceMock.mockReset().mockResolvedValue(SNAPSHOT));

  function renderScreen(units?: typeof UNITS) {
    render(
      <MemoryRouter>
        <OperatorProvider operator={false} securityOperator={false} unitAdminOf={units}>
          <GovernanceScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
  }

  it("Edit is on and opens the editor read only; New profile and Delete stay off", async () => {
    renderScreen(UNITS);
    await screen.findByText(PROFILE.name);
    const edit = screen.getByRole("button", { name: `${GOV.EDIT} ${PROFILE.name}` });
    expect(edit).toBeEnabled();
    expect(screen.getByRole("button", { name: GOV.NEW_CTA })).toBeDisabled();
    expect(screen.getByRole("button", { name: `${GOV.DELETE} ${PROFILE.name}` })).toBeDisabled();
    await userEvent.setup().click(edit);
    expect(await screen.findByRole("button", { name: GOV.SAVE })).toBeDisabled();
  });

  it("without a grant, Edit is off as today", async () => {
    renderScreen();
    await screen.findByText(PROFILE.name);
    expect(screen.getByRole("button", { name: `${GOV.EDIT} ${PROFILE.name}` })).toBeDisabled();
  });
});
