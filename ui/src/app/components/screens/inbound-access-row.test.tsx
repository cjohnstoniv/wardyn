/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Permissions row for the `inbound` feature value (r16 T2-e), on the whole screen.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), warning: vi.fn() } }));

const getPermissionsMock = vi.fn();
vi.mock("../../lib/api/permissions", () => ({
  permissions: { getPermissions: () => getPermissionsMock(), upsertGrant: vi.fn(), deleteGrant: vi.fn(), putEnforcement: vi.fn() },
}));
vi.mock("../../lib/api/people", () => ({ people: { list: () => Promise.resolve({ people: [] }) } }));
vi.mock("../../lib/api/user-types", () => ({ userTypes: { listUserTypes: () => Promise.resolve([]) } }));

import { KIND } from "../../lib/permissions-copy";
import type { CapabilityGrant } from "../../lib/types";
import { aheadByHours } from "../../lib/test-clock";
import { OperatorProvider } from "../wardyn/operator-context";
import { PermissionsScreen } from "./permissions";

function grant(over: Partial<CapabilityGrant> = {}): CapabilityGrant {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    subject_type: "user",
    subject: "alice@corp.example",
    capability: "egress_host",
    value: "*.github.com",
    effect: "allow",
    created_at: aheadByHours(-1),
    created_by: "admin",
    ...over,
  };
}

function renderScreen() {
  return render(
    <MemoryRouter>
      <PermissionsScreen />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getPermissionsMock.mockReset().mockResolvedValue({ grants: [], enforcement: {} });
});

// Restricted (migration 0239): the row lists who is allowed and opens the add-grant form.
describe("PermissionsScreen, who may declare inbound ports", () => {
  const allow = (over: Partial<CapabilityGrant>) =>
    grant({ id: "66666666-6666-6666-6666-666666666666", capability: "feature", value: "inbound", ...over });
  const row = () => screen.findByTestId("inbound-access-row");

  it("says nobody may until an allow names them, and lists the allows, a service by its name", async () => {
    getPermissionsMock.mockResolvedValue({
      grants: [
        allow({ subject_type: "group", subject: "platform" }),
        allow({ id: "77777777-7777-7777-7777-777777777777", subject_type: "user", subject: "service:billing" }),
        allow({ id: "88888888-8888-8888-8888-888888888888", effect: "deny", subject: "mallory@corp.example" }),
      ],
      enforcement: {},
    });
    renderScreen();
    const r = within(await row());
    expect(r.getByRole("heading", { name: "Inbound access" })).toBeInTheDocument();
    expect(r.getByText("By grant only")).toBeInTheDocument();
    expect(r.getByText("Declare ports that other people and services can call through the gateway. Off until you allow it.")).toBeInTheDocument();
    expect(r.getByText("Allowed for:")).toBeInTheDocument();
    expect(r.getByText("Group")).toBeInTheDocument();
    expect(r.getByText("platform")).toBeInTheDocument();
    expect(r.getByText("Service")).toBeInTheDocument();
    expect(r.getByText("billing")).toBeInTheDocument();
    expect(r.queryByText("mallory@corp.example")).toBeNull();
    expect(r.queryByText("Requires the admin or security admin role.")).toBeNull();
  });

  it("shows a service in the grant table by its name, with a Service tag", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [allow({ subject_type: "user", subject: "service:billing" })], enforcement: {} });
    renderScreen();
    await row();
    const cell = within(screen.getByRole("table")).getByText("billing").closest("td") as HTMLElement;
    expect(within(cell).getByText("Service")).toBeInTheDocument();
  });

  it("titles an inbound grant's row Inbound access, and any other feature grant by the feature kind", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [allow({}), allow({ id: "77777777-7777-7777-7777-777777777777", value: "ssh_key" })], enforcement: {} });
    renderScreen();
    await row();
    const cells = within(screen.getByRole("table")).getAllByRole("row").map((r) => within(r).queryByText(/Inbound access|SSH keys and API tokens/)?.textContent);
    expect(cells.filter(Boolean)).toEqual(["Inbound access", "SSH keys and API tokens"]);
    expect(KIND.feature.valueHint).toMatch(/\binbound, self_host_runner and service_attach are never covered by \*/);
  });

  it("Add a grant fills the form's capability and value, again after a second click", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(within(await row()).getByRole("button", { name: "Add a grant" }));
    const value = () => screen.getByLabelText(new RegExp(`^${KIND.feature.valueLabel}`));
    expect(value()).toHaveValue("inbound");
    await user.clear(value());
    await user.click(within(await row()).getByRole("button", { name: "Add a grant" }));
    expect(value()).toHaveValue("inbound");
  });

  it("reads, with its reason, for an admin who is not a security admin", async () => {
    render(
      <MemoryRouter>
        <OperatorProvider operator securityOperator={false}>
          <PermissionsScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    getPermissionsMock.mockResolvedValue({ grants: [], enforcement: {} });
    const r = within(await row());
    expect(r.getByText("Requires the admin or security admin role.")).toBeInTheDocument();
    expect(r.getByRole("button", { name: "Add a grant" })).toBeDisabled();
  });
});
