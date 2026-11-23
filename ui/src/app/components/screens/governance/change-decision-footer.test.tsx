/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The decision footer follows the server's `approvals.viewer`: which buttons are on, what the status line says, and
// when Reject needs a reason. The console decides none of it from a count of its own.
import * as React from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CHANGE_APPROVALS } from "../../../lib/change-review-copy";
import { CHANGES } from "../../../lib/governance-copy";
import { aheadByHours } from "../../../lib/test-clock";
import type { ApprovalViewer, GovernanceChange, GovernanceChangeApprovals } from "../../../lib/types";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { setField } from "../../../../test/set-field";
import { ChangeDecisionFooter, type ChangeDecisionFooterProps } from "./change-decision-footer";

function change(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_unit",
    op: "update",
    target_key: "payments",
    state: "pending",
    proposed_by: "rory@example.com",
    proposed_at: aheadByHours(-2),
    expires_at: aheadByHours(72),
    diff: { changed: [] },
    ...over,
  };
}

function withApprovals(viewer: ApprovalViewer | undefined, over: Partial<GovernanceChangeApprovals> = {}): GovernanceChange {
  return change({ approvals: { required: 3, require_security: true, have: 1, ...(viewer ? { viewer } : {}), ...over } });
}

type Over = Partial<Omit<ChangeDecisionFooterProps, "reason" | "onReason">> & { reason?: string };

function mount(c: GovernanceChange, over: Over = {}) {
  const fns = { onApprove: vi.fn(), onReject: vi.fn(), onTakeBack: vi.fn(), onEditNote: vi.fn() };
  function Harness() {
    const [reason, setReason] = React.useState(over.reason ?? "");
    return (
      <ChangeDecisionFooter
        change={c}
        principal="bo@example.com"
        stale={false}
        busy={false}
        editing={false}
        refused={null}
        pinned={false}
        hasNote={false}
        {...fns}
        {...over}
        reason={reason}
        onReason={setReason}
      />
    );
  }
  render(<Harness />);
  return fns;
}

const approve = () => screen.queryByRole("button", { name: CHANGES.APPROVE });
const reject = () => screen.queryByRole("button", { name: CHANGES.REJECT });

describe("a viewer the server does not name", () => {
  it("the admin token gets Approve and Reject, both on, and no status line", () => {
    mount(withApprovals(undefined));
    expect(approve()).toBeEnabled();
    expect(reject()).toBeEnabled();
    expect(screen.queryByTestId("change-viewer-status")).toBeNull();
  });

  it("not_eligible gets today's Approve and Reject, on, with no status line and no new sentence", () => {
    mount(withApprovals({ state: "not_eligible" }));
    expect(approve()).toBeEnabled();
    expect(reject()).toBeEnabled();
    expect(screen.queryByTestId("change-viewer-status")).toBeNull();
  });

  it("a change with no approvals block is today's footer: Approve and Reject, no status, no rejection note", () => {
    mount(change());
    expect(approve()).toBeEnabled();
    expect(reject()).toBeEnabled();
    expect(screen.queryByTestId("change-viewer-status")).toBeNull();
    expect(screen.queryByTestId("change-reject-note")).toBeNull();
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(screen.getByText(CHANGES.REASON_HINT)).toBeInTheDocument();
  });
});

describe("the status line", () => {
  it("can_approve says what this approval makes the count", () => {
    mount(withApprovals({ state: "can_approve" }));
    expect(screen.getByTestId("change-viewer-status")).toHaveTextContent("Your approval counts 2 of 3.");
    expect(approve()).toBeEnabled();
  });

  it("can_approve with last says the change applies when you approve", () => {
    mount(withApprovals({ state: "can_approve", last: true }));
    expect(screen.getByTestId("change-viewer-status")).toHaveTextContent(CHANGE_APPROVALS.STATUS_LAST);
  });

  it("approved says when and how many more, offers Take back my approval, and has no Approve, Reject or reason", async () => {
    const fns = mount(withApprovals({ state: "approved", approved_at: aheadByHours(-0.1) }, { still_needed: 2 }));
    expect(screen.getByTestId("change-viewer-status")).toHaveTextContent("You approved 6m ago. Waiting for 2 more.");
    expect(approve()).toBeNull();
    expect(reject()).toBeNull();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_APPROVALS.TAKE_BACK }));
    expect(fns.onTakeBack).toHaveBeenCalledTimes(1);
  });

  it("cannot_be_last turns Approve off and leaves Reject on", () => {
    mount(withApprovals({ state: "cannot_be_last" }));
    expect(approve()).toBeDisabled();
    expect(reject()).toBeEnabled();
  });
});

describe("the proposer", () => {
  it("reads the own-change note, Edit note and Withdraw, with no Approve", async () => {
    const fns = mount(withApprovals({ state: "proposer" }), { principal: "rory@example.com", hasNote: true });
    expect(screen.getByText(CHANGES.OWN_NOTE)).toBeInTheDocument();
    expect(approve()).toBeNull();
    expect(reject()).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.EDIT }));
    expect(fns.onEditNote).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: CHANGE_APPROVALS.WITHDRAW }));
    expect(fns.onReject).toHaveBeenCalledTimes(1);
  });

  it("offers Add a note, with its hint, when the change has none", () => {
    mount(withApprovals({ state: "proposer" }), { principal: "rory@example.com" });
    expect(screen.getByRole("button", { name: CHANGE_NOTE.ADD })).toBeInTheDocument();
    expect(screen.getByText(CHANGE_NOTE.NO_NOTE_HINT)).toBeInTheDocument();
  });

  it("withdraws with no reason even where approvers owe one, and the reason box is optional", async () => {
    const fns = mount(withApprovals({ state: "proposer" }, { reject_reason_required: true }), { principal: "rory@example.com" });
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_APPROVALS.WITHDRAW }));
    expect(fns.onReject).toHaveBeenCalledTimes(1);
    expect(screen.queryByTestId("change-reason-missing")).toBeNull();
  });

  it("says only security can decide when every approver is security", () => {
    mount(change({ approvers: [{ kind: "security" }] }), { principal: "rory@example.com" });
    expect(screen.getByText(CHANGE_NOTE.OWN_SECURITY_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(CHANGES.OWN_NOTE)).toBeNull();
    expect(approve()).toBeDisabled();
    expect(reject()).toBeEnabled();
  });
});

describe("the reason", () => {
  it("reads Reason (required to reject) when the server says it is, and today's label when it does not", () => {
    mount(withApprovals({ state: "can_approve" }, { reject_reason_required: true }));
    expect(screen.getByLabelText(CHANGE_APPROVALS.REASON_REQUIRED_LABEL)).toBeInTheDocument();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
  });

  it("keeps today's label and hint when a reason is not required", () => {
    mount(withApprovals({ state: "can_approve" }, { reject_reason_required: false }));
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(screen.getByText(CHANGES.REASON_HINT)).toBeInTheDocument();
  });

  it("an empty reason when one is required shows the sentence and does not reject", async () => {
    const fns = mount(withApprovals({ state: "can_approve" }, { reject_reason_required: true }));
    await userEvent.click(reject()!);
    expect(screen.getByTestId("change-reason-missing")).toHaveTextContent(CHANGE_APPROVALS.REASON_NEEDED);
    expect(fns.onReject).not.toHaveBeenCalled();
    setField(screen.getByLabelText(CHANGE_APPROVALS.REASON_REQUIRED_LABEL), "not now");
    expect(screen.queryByTestId("change-reason-missing")).toBeNull();
    await userEvent.click(reject()!);
    expect(fns.onReject).toHaveBeenCalledTimes(1);
  });

  it("a blank reason of spaces counts as empty", async () => {
    const fns = mount(withApprovals({ state: "can_approve" }, { reject_reason_required: true }), { reason: "   " });
    await userEvent.click(reject()!);
    expect(fns.onReject).not.toHaveBeenCalled();
  });

  it("approving needs no reason", async () => {
    const fns = mount(withApprovals({ state: "can_approve" }, { reject_reason_required: true }));
    await userEvent.click(approve()!);
    expect(fns.onApprove).toHaveBeenCalledTimes(1);
  });
});

describe("under the buttons", () => {
  it("an approver is told that rejecting ends the change for everyone", () => {
    mount(withApprovals({ state: "can_approve" }));
    expect(screen.getByTestId("change-reject-note")).toHaveTextContent(CHANGE_APPROVALS.REJECT_NOTE);
  });

  it("the proposer is not", () => {
    mount(withApprovals({ state: "proposer" }), { principal: "rory@example.com" });
    expect(screen.queryByTestId("change-reject-note")).toBeNull();
  });

  it("a viewer who has approved is not", () => {
    mount(withApprovals({ state: "approved", approved_at: aheadByHours(-1) }));
    expect(screen.queryByTestId("change-reject-note")).toBeNull();
  });
});

describe("a stale change", () => {
  it("shows the sentence and turns both buttons off", () => {
    mount(withApprovals({ state: "can_approve" }), { stale: true });
    expect(screen.getByTestId("change-stale")).toHaveTextContent(
      "This change is stale: payments changed after it was proposed. It can't apply, and its approvals are void. Ask rory@example.com to propose it again.",
    );
    expect(approve()).toBeDisabled();
    expect(reject()).toBeDisabled();
  });

  it("is not shown for a change that is not stale", () => {
    mount(withApprovals({ state: "can_approve" }));
    expect(screen.queryByTestId("change-stale")).toBeNull();
  });
});

describe("a refused decision", () => {
  it("shows the server's sentence verbatim under the frozen heading", () => {
    mount(withApprovals({ state: "can_approve" }), { refused: "the proposer is no longer active" });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(CHANGES.DECIDE_REFUSED_TITLE);
    expect(alert).toHaveTextContent("the proposer is no longer active");
  });
});

describe("a request", () => {
  const request = (over: Partial<GovernanceChange> = {}) =>
    change({ target_kind: "governance_elevation", op: "create", target_key: "e1", ...over });

  it("someone else's keeps Approve and Reject and the reason", () => {
    mount(request());
    expect(approve()).toBeEnabled();
    expect(reject()).toBeEnabled();
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
  });

  it("the person's own reads Withdraw request, with no reason box and Approve off", async () => {
    const fns = mount(request(), { principal: "rory@example.com" });
    expect(screen.getByText(CHANGE_NOTE.OWN_REQUEST)).toBeInTheDocument();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
    expect(approve()).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST }));
    expect(fns.onReject).toHaveBeenCalledTimes(1);
  });
});

describe("the layout", () => {
  it("pins the buttons to a bar at the bottom below 640 px, and keeps the reason in the flow above it", () => {
    mount(withApprovals({ state: "can_approve" }), { pinned: true });
    const bar = screen.getByTestId("change-decision-bar");
    expect(bar).toHaveAttribute("data-pinned", "true");
    expect(bar.className).toContain("fixed");
    expect(bar).toContainElement(approve());
    expect(bar).toContainElement(reject());
    expect(bar).not.toContainElement(screen.getByLabelText(CHANGES.REASON_LABEL));
  });

  it("leaves the buttons in the flow when wide", () => {
    mount(withApprovals({ state: "can_approve" }));
    const bar = screen.getByTestId("change-decision-bar");
    expect(bar).not.toHaveAttribute("data-pinned");
    expect(bar.className).not.toContain("fixed");
  });

  it("gives way to the note form while it is open", () => {
    mount(withApprovals({ state: "proposer" }), { principal: "rory@example.com", editing: true });
    expect(screen.queryByTestId("change-decision-bar")).toBeNull();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
    expect(screen.queryByRole("button", { name: CHANGE_NOTE.ADD })).toBeNull();
  });
});
