/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Requests on the Changes tab: an elevation request reads as a request (its length line, its person → unit
// target, a three-row page), a person's own request offers Withdraw request, and the kinds that had no
// label now have one. Strings are read by constant.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: { get: vi.fn(), members: vi.fn() },
}));

const getChangeMock = vi.fn();
const rejectMock = vi.fn();
const approveMock = vi.fn();
const impactMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      getChange: (...a: unknown[]) => getChangeMock(...a),
      rejectChange: (...a: unknown[]) => rejectMock(...a),
      approveChange: (...a: unknown[]) => approveMock(...a),
      getChangeImpact: (...a: unknown[]) => impactMock(...a),
    },
  };
});

import { CHANGES } from "../../../lib/governance-copy";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange } from "../../../lib/types";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ChangeReviewPage } from "./change-review-page";
import { ChangesTab } from "./changes";

function elevation(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "e1",
    target_kind: "governance_elevation",
    op: "create",
    target_key: "e1",
    state: "pending",
    proposed_by: "alice",
    proposed_at: aheadByHours(-1),
    expires_at: aheadByHours(23),
    diff: { after: { unit_id: "u1", unit_name: "Payments", subject: "alice", duration: "4h" }, changed: ["duration"] },
    note: { reason: "Production incident", ticket: "SUP-1" },
    ...over,
  };
}

function image(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    ...elevation(),
    id: "i1",
    target_kind: "governance_image_request",
    target_key: "i1",
    diff: { after: { image: "registry.example/app:1", unit_id: "u1", unit_name: "Payments", subject: "alice" }, changed: ["image"] },
    note: undefined,
    ...over,
  };
}

function renderTab(changes: GovernanceChange[], principal = "bo@example.com") {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} securityOperator={false} principal={principal}>
        <ChangesTab changes={changes} loadError={null} onChanged={() => {}} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

// The change on its own page, as the row opens it.
function renderPage(c: GovernanceChange, principal = "bo@example.com") {
  getChangeMock.mockResolvedValue(c);
  render(
    <MemoryRouter initialEntries={[`/admin/governance/changes/${c.id}`]}>
      <OperatorProvider operator={false} securityOperator={false} principal={principal}>
        <Routes>
          <Route path="/admin/governance/changes/:id" element={<ChangeReviewPage />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getChangeMock.mockReset();
  rejectMock.mockReset();
  approveMock.mockReset();
  impactMock.mockReset();
  impactMock.mockResolvedValue({ computed: "none", not_computed: "kind", as_of: aheadByHours(0) });
});

describe("an elevation request in the list", () => {
  it("reads as a request: label, length line with the ticket, and the person → unit target", () => {
    renderTab([elevation()]);
    const row = screen.getByRole("row", { name: /Elevation · New/ });
    expect(within(row).getByRole("button", { name: "Elevation · New" })).toBeInTheDocument();
    expect(row.querySelector("p")).toHaveTextContent("4 hours from approval · SUP-1");
    expect(within(row).getByText("alice → Payments")).toBeInTheDocument();
    expect(within(row).getByText("alice → Payments").className).not.toContain("font-mono");
    // The reason is not in the list line for a request: the length takes its place.
    expect(row).not.toHaveTextContent("Production incident");
  });

  it("leaves the ticket off when there is none", () => {
    renderTab([elevation({ note: undefined })]);
    expect(screen.getByRole("row", { name: /Elevation · New/ }).querySelector("p")).toHaveTextContent(/^4 hours from approval$/);
  });

  it("falls back to the generic row when a field is missing", () => {
    renderTab([elevation({ diff: { after: { subject: "alice" }, changed: [] } })]);
    const row = screen.getByRole("row", { name: /Elevation · New/ });
    expect(within(row).getByText("e1")).toBeInTheDocument();
    expect(row).not.toHaveTextContent("from approval");
    expect(row.querySelector("p")).toHaveTextContent("Production incident SUP-1");
  });
});

describe("an elevation request's page", () => {
  it("shows Person, Unit and Length rows, the target in the title, and keeps the full diff", async () => {
    renderPage(elevation());
    expect(await screen.findByRole("heading", { level: 1, name: /Elevation · New\s*alice → Payments/ })).toBeInTheDocument();
    const rows = screen.getAllByRole("row").map((r) => r.textContent);
    expect(rows).toEqual([
      `${CHANGE_NOTE.ELEV_PERSON}alice`,
      `${CHANGE_NOTE.ELEV_UNIT}Payments`,
      `${CHANGE_NOTE.ELEV_LENGTH}${CHANGE_NOTE.ELEV_LENGTH_VALUE("4 hours")}`,
    ]);
    expect(screen.getByText(CHANGES.DIFF_FULL)).toBeInTheDocument();
    expect(screen.getByTestId("change-note")).toHaveTextContent("Production incident");
  });

  it("someone else's request keeps Reject and Approve and the decider's reason", async () => {
    renderPage(elevation());
    expect(await screen.findByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: CHANGES.REJECT })).toBeEnabled();
    expect(screen.getByRole("button", { name: CHANGES.APPROVE })).toBeEnabled();
    expect(screen.queryByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST })).toBeNull();
  });
});

describe("a person's own request", () => {
  it("reads Withdraw request with the request note, no reason field, and Approve off", async () => {
    renderPage(elevation(), "alice");
    expect(await screen.findByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST })).toBeEnabled();
    expect(screen.queryByRole("button", { name: CHANGES.REJECT })).toBeNull();
    expect(screen.getByText(CHANGE_NOTE.OWN_REQUEST)).toBeInTheDocument();
    expect(screen.queryByText(CHANGES.OWN_NOTE)).toBeNull();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
    expect(screen.getByRole("button", { name: CHANGES.APPROVE })).toBeDisabled();
  });

  it("Withdraw request posts a rejection with no reason", async () => {
    rejectMock.mockResolvedValue({});
    renderPage(elevation(), "alice");
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST }));
    expect(rejectMock).toHaveBeenCalledWith("e1", "");
  });

  it("an own image request reads the same way", async () => {
    renderPage(image(), "alice");
    expect(await screen.findByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST })).toBeInTheDocument();
    expect(screen.getByText(CHANGE_NOTE.OWN_REQUEST)).toBeInTheDocument();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
  });

  it("an own ordinary change keeps Reject, the reason field and OWN_NOTE", async () => {
    const own = elevation({ id: "g1", target_kind: "governance_unit", op: "update", target_key: "team-a", diff: { changed: [] } });
    renderPage(own, "alice");
    expect(await screen.findByRole("button", { name: CHANGES.REJECT })).toBeInTheDocument();
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(screen.getByText(CHANGES.OWN_NOTE)).toBeInTheDocument();
  });
});

describe("kind labels", () => {
  it("an image request reads Image · New", () => {
    renderTab([image()]);
    expect(screen.getByRole("button", { name: "Image · New" })).toBeInTheDocument();
  });

  it("the deciders read Deciders · Set", () => {
    renderTab([elevation({ id: "d1", target_kind: "governance_unit_deciders", op: "upsert", target_key: "u1", diff: { changed: [] } })]);
    expect(screen.getByRole("button", { name: "Deciders · Set" })).toBeInTheDocument();
  });

  it("a unit-admin change starts with Unit admin and an approval rule with Approval rule", () => {
    renderTab([
      elevation({ id: "a1", target_kind: "governance_unit_admin", op: "create", target_key: "x", diff: { changed: [] } }),
      elevation({ id: "r1", target_kind: "approval_rule", op: "create", target_key: "y", diff: { changed: [] } }),
    ]);
    const labels = screen.getAllByRole("row").slice(1).map((r) => within(r).getAllByRole("button")[0].textContent ?? "");
    expect(labels[0].startsWith("Unit admin")).toBe(true);
    expect(labels[1].startsWith("Approval rule")).toBe(true);
  });
});
