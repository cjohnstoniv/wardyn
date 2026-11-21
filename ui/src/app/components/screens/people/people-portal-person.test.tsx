/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A portal person on the People page: the issuer and internal address columns, the role, and no
// suspend or purge control. The strings are written out so a drift in the copy module fails here.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { PersonSummary } from "../../../lib/types";

const listMock = vi.fn();
const getMock = vi.fn();
vi.mock("../../../lib/api/people", () => ({
  people: {
    list: (...a: unknown[]) => listMock(...a),
    get: (...a: unknown[]) => getMock(...a),
    create: vi.fn(),
    tokens: () => Promise.resolve([]),
    signOutEverywhere: vi.fn(),
    removeSSHKeys: vi.fn(),
  },
}));
vi.mock("../../../lib/api/credentials", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/credentials")>()),
  credentials: { erase: vi.fn() },
}));
vi.mock("../../../lib/api/access", () => ({
  access: { getAccess: () => Promise.reject(new Error("sso off")) },
  AccessCollisionError: class extends Error {},
  AccessPostureFlipRequiredError: class extends Error {},
}));
vi.mock("../../../lib/api/scim", () => ({
  scim: { getStatus: () => Promise.resolve({ configured: false, last_token_slot: "", purge_after_seconds: 0, keep_workspaces: false, deactivated: [], pending: [], drives: [] }) },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { OperatorProvider } from "../../wardyn/operator-context";
import { PeopleScreen } from "./people-screen";

function person(over: Partial<PersonSummary> = {}): PersonSummary {
  return {
    principal: "sub-ana",
    email: "ana@example.com",
    issuer_kind: "oidc",
    pre_created: false,
    last_signed_in_at: new Date(Date.now() - 3_600_000).toISOString(),
    role: "admin",
    active_sessions: 0,
    api_tokens: 0,
    ssh_keys: 0,
    credentials: 0,
    active_runs: 0,
    ...over,
  };
}

const PORTAL = person({
  principal: "delegate:person:k1:sub-sam",
  email: "sam@example.com",
  role: "user",
  issuer: "https://idp.example.com/oauth2/default",
  internal_address: "https://idp.internal.example.com/oauth2/default",
});
const ROWS = [person(), PORTAL];

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator operatorResolved principal="admin@corp.example">
        <PeopleScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  listMock.mockReset();
  getMock.mockReset();
  listMock.mockResolvedValue({ people: ROWS });
  getMock.mockImplementation((principal: string) => Promise.resolve(ROWS.find((r) => r.principal === principal)));
});

describe("a portal person on the people page", () => {
  it("shows the issuer, internal address and user role", async () => {
    renderScreen();
    const sam = (await screen.findByText("sam@example.com")).closest("tr")!;
    expect(within(sam).getByText("https://idp.example.com/oauth2/default")).toBeInTheDocument();
    expect(within(sam).getByText("https://idp.internal.example.com/oauth2/default")).toBeInTheDocument();
    expect(within(sam).getByText("User")).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Issuer" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Internal address" })).toBeInTheDocument();
  });

  it("shows a dash in both address columns for a workforce row", async () => {
    renderScreen();
    const ana = (await screen.findByText("ana@example.com")).closest("tr")!;
    // Issuer, internal address and the empty Holds cell.
    expect(within(ana).getAllByText("—")).toHaveLength(3);
    expect(within(ana).getByText("Admin")).toBeInTheDocument();
  });

  it("shows no suspend or purge button", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "sam@example.com" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText("Portal person")).toBeInTheDocument();
    expect(within(drawer).getByText("https://idp.internal.example.com/oauth2/default")).toBeInTheDocument();
    for (const name of [/suspend/i, /reactivate/i, /purge/i]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
  });
});
