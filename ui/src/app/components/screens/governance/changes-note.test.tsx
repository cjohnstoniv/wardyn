/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The change note on the Changes tab: the note line in the list, the "Why this change" block and the
// proposer's Add/Edit note in the drawer, the security-only variant of the own-change note, and the
// "Only security can decide" filter. Strings are read by constant.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: { get: vi.fn(), members: vi.fn() },
}));

const getChangeMock = vi.fn();
const listChangesMock = vi.fn();
const noteMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      getChange: (...a: unknown[]) => getChangeMock(...a),
      listChanges: (...a: unknown[]) => listChangesMock(...a),
      noteChange: (...a: unknown[]) => noteMock(...a),
    },
  };
});

import { CHANGES, GOVERNANCE } from "../../../lib/governance-copy";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange } from "../../../lib/types";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ChangesTab } from "./changes";

const NOTE = { reason: "Raise the build profile's memory cap", ticket: "CHG-4821" };

function change(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_unit",
    op: "update",
    target_key: "team-a",
    state: "pending",
    proposed_by: "ana@example.com",
    proposed_at: aheadByHours(-2),
    expires_at: aheadByHours(20),
    diff: { before: { name: "team-a", limits: { mem: 1 } }, after: { name: "team-a", limits: { mem: 2 } }, changed: ["limits.mem"] },
    ...over,
  };
}

function ui(changes: GovernanceChange[], o: { principal?: string; security?: boolean; onChanged?: () => void; loadError?: string | null } = {}) {
  return (
    <MemoryRouter>
      <OperatorProvider operator={false} securityOperator={o.security ?? false} principal={o.principal ?? "bo@example.com"}>
        <ChangesTab changes={changes} loadError={o.loadError ?? null} onChanged={o.onChanged ?? (() => {})} />
      </OperatorProvider>
    </MemoryRouter>
  );
}

const openRow = (label = "Profile · Edit") => userEvent.click(screen.getAllByRole("button", { name: label })[0]);

beforeEach(() => {
  getChangeMock.mockReset();
  listChangesMock.mockReset();
  noteMock.mockReset();
  vi.mocked(toast.success).mockReset();
});

describe("the note line in the list", () => {
  it("shows the reason then the ticket in mono under the label", () => {
    render(ui([change({ note: NOTE })]));
    const row = screen.getByRole("row", { name: /Profile · Edit/ });
    const line = row.querySelector("p") as HTMLElement;
    expect(line).toHaveTextContent(`${NOTE.reason} ${NOTE.ticket}`);
    expect(within(line).getByText(NOTE.ticket).className).toContain("font-mono");
    expect(within(line).getByText(NOTE.reason).className).toContain("truncate");
  });

  it("shows only the reason when there is no ticket, and nothing when there is no note", () => {
    render(ui([change({ id: "a", note: { reason: "Only a reason" } }), change({ id: "b", target_key: "team-b" })]));
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows[0].querySelector("p")).toHaveTextContent("Only a reason");
    expect(rows[1].querySelector("p")).toBeNull();
  });
});

describe("the drawer's Why this change block", () => {
  it("sits after the amber notes and before the decider's reason, as plain text", async () => {
    const c = change({
      note: { reason: "<b>not html</b> https://example.com", ticket: "https://example.com/t/1" },
      diff: { ...change().diff, notes: ["omits the egress list"] },
    });
    render(ui([c]));
    await openRow();
    const block = await screen.findByTestId("change-note");
    expect(within(block).getByRole("heading", { name: CHANGE_NOTE.WHY_TITLE })).toBeInTheDocument();
    expect(block).toHaveTextContent("<b>not html</b> https://example.com");
    expect(block.querySelector("a, b")).toBeNull();
    expect(within(block).getByText("https://example.com/t/1").className).toContain("font-mono");
    const amber = screen.getByText(GOVERNANCE.OMISSION_TITLE);
    const reason = screen.getByLabelText(CHANGES.REASON_LABEL);
    expect(amber.compareDocumentPosition(block) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(block.compareDocumentPosition(reason) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("draws no block without a note", async () => {
    render(ui([change()]));
    await openRow();
    await screen.findByLabelText(CHANGES.REASON_LABEL);
    expect(screen.queryByTestId("change-note")).toBeNull();
  });
});

describe("Add a note and Edit note", () => {
  it("is offered only to the proposer", async () => {
    getChangeMock.mockResolvedValue(change());
    render(ui([change()]));
    await openRow();
    await screen.findByLabelText(CHANGES.REASON_LABEL);
    expect(screen.queryByRole("button", { name: CHANGE_NOTE.ADD })).toBeNull();
    expect(screen.queryByRole("button", { name: CHANGE_NOTE.EDIT })).toBeNull();
  });

  it("reads Add a note with the hint when there is no note, and Edit note when there is one", async () => {
    getChangeMock.mockResolvedValue(change());
    const { unmount } = render(ui([change()], { principal: "ana@example.com" }));
    await openRow();
    expect(await screen.findByRole("button", { name: CHANGE_NOTE.ADD })).toBeInTheDocument();
    expect(screen.getByText(CHANGE_NOTE.NO_NOTE_HINT)).toBeInTheDocument();
    unmount();
    render(ui([change({ note: NOTE })], { principal: "ana@example.com" }));
    await openRow();
    expect(await screen.findByRole("button", { name: CHANGE_NOTE.EDIT })).toBeInTheDocument();
    expect(screen.queryByText(CHANGE_NOTE.NO_NOTE_HINT)).toBeNull();
  });

  it("opens the form in place and hides the decider's reason and the buttons while it is open", async () => {
    getChangeMock.mockResolvedValue(change());
    render(ui([change()], { principal: "ana@example.com" }));
    await openRow();
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_NOTE.ADD }));
    expect(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL)).toBeInTheDocument();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
    expect(screen.queryByRole("button", { name: CHANGES.REJECT })).toBeNull();
    expect(screen.queryByRole("button", { name: CHANGES.APPROVE })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.CANCEL }));
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: CHANGES.REJECT })).toBeInTheDocument();
  });

  it("a save toasts, shows the returned note in the drawer and reloads the list", async () => {
    getChangeMock.mockResolvedValue(change());
    noteMock.mockResolvedValue(change({ note: NOTE }));
    const onChanged = vi.fn();
    render(ui([change()], { principal: "ana@example.com", onChanged }));
    await openRow();
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_NOTE.ADD }));
    await userEvent.type(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL), NOTE.reason);
    await userEvent.type(screen.getByLabelText(CHANGE_NOTE.TICKET_LABEL), NOTE.ticket);
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(noteMock).toHaveBeenCalledWith("c1", NOTE);
    expect(toast.success).toHaveBeenCalledWith(CHANGE_NOTE.SAVED);
    const block = await screen.findByTestId("change-note");
    expect(block).toHaveTextContent(NOTE.reason);
    expect(screen.getByRole("button", { name: CHANGE_NOTE.EDIT })).toBeInTheDocument();
    expect(onChanged).toHaveBeenCalled();
  });

  it("a 404 on save reloads the list and closes the drawer; a 409 reloads and keeps the refusal", async () => {
    const { HttpError } = await import("../../../lib/api/core");
    getChangeMock.mockResolvedValue(change());
    noteMock.mockRejectedValueOnce(new HttpError(409, "no longer pending")).mockRejectedValueOnce(new HttpError(404, "gone"));
    const onChanged = vi.fn();
    render(ui([change()], { principal: "ana@example.com", onChanged }));
    await openRow();
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_NOTE.ADD }));
    await userEvent.type(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL), "x");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(await screen.findByText("no longer pending")).toBeInTheDocument();
    expect(onChanged).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(onChanged).toHaveBeenCalledTimes(2);
  });
});

describe("an own change that only security can decide", () => {
  const security = { kind: "security" } as const;

  it("shows the security-only note in place of OWN_NOTE when every approver is security", async () => {
    getChangeMock.mockResolvedValue(change({ approvers: [security, security] }));
    render(ui([change()], { principal: "ana@example.com" }));
    await openRow();
    expect(await screen.findByText(CHANGE_NOTE.OWN_SECURITY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(CHANGE_NOTE.OWN_SECURITY_BODY)).toBeInTheDocument();
    expect(screen.queryByText(CHANGES.OWN_NOTE)).toBeNull();
    expect(getChangeMock).toHaveBeenCalledWith("c1");
  });

  it("keeps OWN_NOTE when anyone but security can decide, when nobody is listed, and when the read fails", async () => {
    for (const read of [
      () => Promise.resolve(change({ approvers: [security, { kind: "unit_admin", unit_name: "Payments" }] })),
      () => Promise.resolve(change({ approvers: [] })),
      () => Promise.reject(new Error("offline")),
    ]) {
      getChangeMock.mockReset();
      getChangeMock.mockImplementation(read);
      const { unmount } = render(ui([change()], { principal: "ana@example.com" }));
      await openRow();
      await vi.waitFor(() => expect(getChangeMock).toHaveBeenCalled());
      expect(await screen.findByText(CHANGES.OWN_NOTE)).toBeInTheDocument();
      expect(screen.queryByText(CHANGE_NOTE.OWN_SECURITY_TITLE)).toBeNull();
      unmount();
    }
  });

  it("does not read the change for someone else's proposal", async () => {
    render(ui([change()]));
    await openRow();
    await screen.findByLabelText(CHANGES.REASON_LABEL);
    expect(getChangeMock).not.toHaveBeenCalled();
  });
});

describe("the Only security can decide filter", () => {
  it("is drawn only for a security operator", () => {
    const { unmount } = render(ui([change()], { security: false }));
    expect(screen.queryByRole("radiogroup", { name: CHANGE_NOTE.FILTER_LABEL })).toBeNull();
    unmount();
    render(ui([change()], { security: true }));
    const group = screen.getByRole("radiogroup", { name: CHANGE_NOTE.FILTER_LABEL });
    expect(within(group).getByRole("radio", { name: CHANGE_NOTE.FILTER_ALL })).toBeChecked();
    expect(within(group).getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY })).not.toBeChecked();
  });

  it("reads the filtered list itself, shows it in place of the prop, and re-reads when the prop changes", async () => {
    const mine = change({ id: "s1", target_kind: "governance_envelope", target_key: "root" });
    listChangesMock.mockResolvedValue([mine]);
    const view = render(ui([change(), mine], { security: true }));
    await userEvent.click(screen.getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY }));
    expect(listChangesMock).toHaveBeenCalledWith({ approver: "security" });
    await vi.waitFor(() => expect(screen.queryByRole("button", { name: "Profile · Edit" })).toBeNull());
    expect(screen.getByRole("button", { name: "Platform envelope · Edit" })).toBeInTheDocument();
    expect(listChangesMock).toHaveBeenCalledTimes(1);
    view.rerender(ui([change(), mine, change({ id: "c9" })], { security: true }));
    await vi.waitFor(() => expect(listChangesMock).toHaveBeenCalledTimes(2));
  });

  it("does not flash the empty state while the filtered read is in flight", async () => {
    let finish!: (c: GovernanceChange[]) => void;
    listChangesMock.mockReturnValue(new Promise<GovernanceChange[]>((r) => (finish = r)));
    render(ui([change()], { security: true }));
    await userEvent.click(screen.getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY }));
    expect(screen.queryByText(CHANGE_NOTE.FILTERED_EMPTY_TITLE)).toBeNull();
    finish([]);
    expect(await screen.findByText(CHANGE_NOTE.FILTERED_EMPTY_TITLE)).toBeInTheDocument();
  });

  it("an empty filtered list shows the filtered empty state, and Show all changes turns the pill off", async () => {
    listChangesMock.mockResolvedValue([]);
    render(ui([change()], { security: true }));
    await userEvent.click(screen.getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY }));
    expect(await screen.findByText(CHANGE_NOTE.FILTERED_EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(CHANGE_NOTE.FILTERED_EMPTY_BODY)).toBeInTheDocument();
    expect(screen.getByRole("radiogroup", { name: CHANGE_NOTE.FILTER_LABEL })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.FILTERED_EMPTY_ACTION }));
    expect(screen.getByRole("radio", { name: CHANGE_NOTE.FILTER_ALL })).toBeChecked();
    expect(screen.getByRole("button", { name: "Profile · Edit" })).toBeInTheDocument();
  });

  it("a failed filtered read shows the load-error note with the pill still on", async () => {
    listChangesMock.mockRejectedValue(new Error("the list is unavailable"));
    render(ui([change()], { security: true }));
    await userEvent.click(screen.getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the list is unavailable");
    expect(screen.getByRole("radio", { name: CHANGE_NOTE.FILTER_SECURITY })).toBeChecked();
  });
});
