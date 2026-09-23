/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// UserTypesScreen — the security admin's list/editor surface for org-defined
// user types (0.8, UT-7a). Same conventions governance-screen.test.tsx uses:
// every assertion reads its expected string from the copy module, and the
// four state pins there apply here too (one `default` button, delete refused
// with the server's own message).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listUserTypesMock = vi.fn();
const createUserTypeMock = vi.fn();
const updateUserTypeMock = vi.fn();
const deleteUserTypeMock = vi.fn();
vi.mock("../../../lib/api/user-types", () => ({
  userTypes: {
    listUserTypes: (...a: unknown[]) => listUserTypesMock(...a),
    createUserType: (...a: unknown[]) => createUserTypeMock(...a),
    updateUserType: (...a: unknown[]) => updateUserTypeMock(...a),
    deleteUserType: (...a: unknown[]) => deleteUserTypeMock(...a),
  },
}));

const getGovernanceMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return { ...actual, governance: { getGovernance: () => getGovernanceMock() } };
});

const explainCapabilitiesMock = vi.fn();
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return { ...actual, permissions: { explainCapabilities: (...a: unknown[]) => explainCapabilitiesMock(...a) } };
});

import { HttpError } from "../../../lib/api/core";
import type { UserType } from "../../../lib/types";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { USER_TYPES as UT } from "../../../lib/user-types-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { SECURITY_ONLY_REASON } from "../../wardyn/copy";
import { UserTypesScreen } from "./user-types-screen";

function type(over: Partial<UserType> = {}): UserType {
  return {
    id: "portfolio-manager",
    name: "Portfolio manager",
    description: "Reads risk models, no repo access.",
    priority: 10,
    built_in: false,
    created_at: "2026-09-20T00:00:00Z",
    updated_at: "2026-09-20T00:00:00Z",
    ...over,
  };
}

const STANDARD = type({ id: "standard", name: "Standard user", description: "", priority: 0, built_in: true });

function renderScreen(list: UserType[] | null = [STANDARD]) {
  if (list) listUserTypesMock.mockResolvedValue(list);
  else listUserTypesMock.mockRejectedValue(new Error("boom"));
  render(<UserTypesScreen />);
}

beforeEach(() => {
  cleanup();
  listUserTypesMock.mockReset();
  createUserTypeMock.mockReset();
  updateUserTypeMock.mockReset();
  deleteUserTypeMock.mockReset();
  getGovernanceMock.mockReset();
  getGovernanceMock.mockResolvedValue({ profiles: [], assignments: [] });
  explainCapabilitiesMock.mockReset();
  explainCapabilitiesMock.mockResolvedValue({ subject_type: "user_type", subject: "", kinds_version: 1, rows: [] });
});

describe("UserTypesScreen — states", () => {
  it("renders the frozen page header", async () => {
    renderScreen();
    expect(await screen.findByRole("heading", { name: UT.TITLE, level: 1 })).toBeInTheDocument();
    expect(screen.getByText(UT.LEAD)).toBeInTheDocument();
  });

  it("fetch_failed offers Retry and is distinct from empty", async () => {
    renderScreen(null);
    expect(await screen.findByText(UT.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(UT.EMPTY_TITLE)).not.toBeInTheDocument();

    listUserTypesMock.mockResolvedValue([STANDARD]);
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText(STANDARD.name)).toBeInTheDocument();
  });

  it("no custom types: the empty state carries New type", async () => {
    renderScreen([]);
    expect(await screen.findByText(UT.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(UT.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: UT.NEW_CTA }).length).toBeGreaterThan(0);
  });

  it("the built-in type carries its badge and delete is refused with a title, not a click", async () => {
    renderScreen([STANDARD, type()]);
    expect(await screen.findByText(STANDARD.name)).toBeInTheDocument();
    expect(screen.getByText(UT.BUILT_IN_BADGE)).toBeInTheDocument();
    const del = screen.getByRole("button", { name: `${UT.DELETE} ${STANDARD.name}` });
    expect(del).toBeDisabled();
    expect(del).toHaveAttribute("title", UT.DELETE_BUILTIN);
  });
});

describe("UserTypesScreen — create", () => {
  it("opens the editor, saves, and reloads the list", async () => {
    renderScreen([STANDARD]);
    await screen.findByText(STANDARD.name);
    await userEvent.click(screen.getByRole("button", { name: UT.NEW_CTA }));
    expect(screen.getByTestId("user-type-editor")).toBeInTheDocument();

    createUserTypeMock.mockResolvedValue(type());
    listUserTypesMock.mockResolvedValue([STANDARD, type()]);
    await userEvent.type(screen.getByLabelText(UT.FIELD_NAME), "Portfolio manager");
    await userEvent.click(screen.getByRole("button", { name: UT.SAVE }));

    expect(createUserTypeMock).toHaveBeenCalledWith(
      expect.objectContaining({ name: "Portfolio manager", priority: 0 }),
    );
    expect(await screen.findByText("Portfolio manager")).toBeInTheDocument();
    // The editor closes on a successful save — one `default` button at a time.
    expect(screen.queryByTestId("user-type-editor")).not.toBeInTheDocument();
  });

  it("a save failure keeps the editor open and shows the server's own message", async () => {
    renderScreen([STANDARD]);
    await screen.findByText(STANDARD.name);
    await userEvent.click(screen.getByRole("button", { name: UT.NEW_CTA }));
    createUserTypeMock.mockRejectedValue(new HttpError(409, 'A user type with the id "x" already exists.'));
    await userEvent.type(screen.getByLabelText(UT.FIELD_NAME), "X");
    await userEvent.click(screen.getByRole("button", { name: UT.SAVE }));
    expect(await screen.findByText('A user type with the id "x" already exists.')).toBeInTheDocument();
    expect(screen.getByTestId("user-type-editor")).toBeInTheDocument();
  });
});

describe("UserTypesScreen — edit reads Ceiling and What this type gets", () => {
  it("shows the assigned profile's name when one binds this type", async () => {
    const t = type();
    getGovernanceMock.mockResolvedValue({
      profiles: [{ id: "prof-1", name: "Portfolio", ceiling: {}, limits: {}, created_at: "", updated_at: "" }],
      assignments: [
        { id: "a1", subject_type: "user_type", subject: t.id, profile_id: "prof-1", priority: 0, created_at: "" },
      ],
    });
    renderScreen([t]);
    await screen.findByText(t.name);
    await userEvent.click(screen.getByRole("button", { name: `${UT.EDIT} ${t.name}` }));
    expect(await screen.findByText(UT.CEILING_PROFILE("Portfolio"))).toBeInTheDocument();
  });

  it("renders the bound profile's run limits read-only, in Governance's labels", async () => {
    const t = type();
    getGovernanceMock.mockResolvedValue({
      profiles: [
        {
          id: "prof-1",
          name: "Portfolio",
          ceiling: {},
          limits: { deny_interactive: true, max_concurrent_runs: 3, max_drive_size_mib: 2048 },
          created_at: "",
          updated_at: "",
        },
      ],
      assignments: [
        { id: "a1", subject_type: "user_type", subject: t.id, profile_id: "prof-1", priority: 0, created_at: "" },
      ],
    });
    renderScreen([t]);
    await screen.findByText(t.name);
    await userEvent.click(screen.getByRole("button", { name: `${UT.EDIT} ${t.name}` }));
    expect(await screen.findByText(GOV.LIMIT_INTERACTIVE_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_QUOTA_LABEL(3))).toBeInTheDocument();
    expect(screen.getByText(`${GOV.LIMIT_DRIVE_SIZE_LABEL}: 2048`)).toBeInTheDocument();
    expect(screen.queryByText(GOV.LIMITS_NONE)).not.toBeInTheDocument();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  });

  it("a bound profile with no limits reads None", async () => {
    const t = type();
    getGovernanceMock.mockResolvedValue({
      profiles: [{ id: "prof-1", name: "Portfolio", ceiling: {}, limits: {}, created_at: "", updated_at: "" }],
      assignments: [
        { id: "a1", subject_type: "user_type", subject: t.id, profile_id: "prof-1", priority: 0, created_at: "" },
      ],
    });
    renderScreen([t]);
    await screen.findByText(t.name);
    await userEvent.click(screen.getByRole("button", { name: `${UT.EDIT} ${t.name}` }));
    expect(await screen.findByText(GOV.LIMITS_NONE)).toBeInTheDocument();
  });

  it("no assignment: names the fallback rather than a blank section", async () => {
    const t = type();
    renderScreen([t]);
    await screen.findByText(t.name);
    await userEvent.click(screen.getByRole("button", { name: `${UT.EDIT} ${t.name}` }));
    expect(await screen.findByText(UT.CEILING_NONE)).toBeInTheDocument();
  });

  it("asks Explain for this type's subject and renders its rows", async () => {
    const t = type();
    explainCapabilitiesMock.mockResolvedValue({
      subject_type: "user_type",
      subject: t.id,
      kinds_version: 1,
      rows: [{ kind: "egress_host", value: "*", state: "blocked" }],
    });
    renderScreen([t]);
    await screen.findByText(t.name);
    await userEvent.click(screen.getByRole("button", { name: `${UT.EDIT} ${t.name}` }));
    expect(explainCapabilitiesMock).toHaveBeenCalledWith("user_type", t.id, expect.any(Array));
    expect(await screen.findByText(UT.EXPLAIN_STATE.blocked)).toBeInTheDocument();
    expect(screen.getByText(UT.WALL_WARNING)).toBeInTheDocument();
  });
});

describe("UserTypesScreen — delete", () => {
  it("a 409 from the five-source guard renders verbatim, count-free", async () => {
    const t = type();
    renderScreen([t]);
    await screen.findByText(t.name);
    await userEvent.click(screen.getByRole("button", { name: `${UT.DELETE} ${t.name}` }));
    deleteUserTypeMock.mockRejectedValue(new HttpError(409, "2 role mappings still name this type."));
    await userEvent.click(screen.getAllByRole("button", { name: UT.DELETE }).at(-1)!);
    expect(await screen.findByText("2 role mappings still name this type.")).toBeInTheDocument();
  });
});

describe("UserTypesScreen — the write gate", () => {
  it("a caller without the security tier gets every write control disabled", async () => {
    // OperatorProvider defaults securityOperator TRUE (fail-open), so the
    // restricted case is the one worth pinning — governance-screen.test.tsx's
    // own precedent.
    listUserTypesMock.mockResolvedValue([STANDARD]);
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <UserTypesScreen />
      </OperatorProvider>,
    );
    await screen.findByText(STANDARD.name);
    expect(screen.getByRole("button", { name: UT.NEW_CTA })).toBeDisabled();
    expect(screen.getByRole("button", { name: `${UT.EDIT} ${STANDARD.name}` })).toBeDisabled();
  });
});

describe("UserTypesScreen — forbidden", () => {
  it("a 403 read shows the security-only reason, not a bare error", async () => {
    listUserTypesMock.mockRejectedValue(new HttpError(403, "forbidden"));
    render(<UserTypesScreen />);
    expect(await screen.findByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(screen.queryByText(UT.FETCH_FAILED_TITLE)).not.toBeInTheDocument();
  });
});
