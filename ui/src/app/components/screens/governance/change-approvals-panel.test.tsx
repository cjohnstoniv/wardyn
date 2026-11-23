/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The approvals panel: what the server's approvals block says, in the words of CHANGE_APPROVALS. The panel counts
// nothing and derives no reason, so each case feeds it a block and reads the sentence back.
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { CHANGE_APPROVALS } from "../../../lib/change-review-copy";
import { shortTime } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange, GovernanceChangeApprovals } from "../../../lib/types";
import { ChangeApprovalsPanel } from "./change-approvals-panel";

function approvals(over: Partial<GovernanceChangeApprovals> = {}): GovernanceChangeApprovals {
  return { required: 2, require_security: true, have: 1, ...over };
}

function change(over: Partial<GovernanceChange> = {}, a?: GovernanceChangeApprovals): GovernanceChange {
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
    ...(a ? { approvals: a } : {}),
    ...over,
  };
}

const SAM = {
  approver: "sam@example.com",
  tier: "security_admin" as const,
  approved_at: aheadByHours(-1),
  counts: true,
};

function panel(c: GovernanceChange, principal = "bo@example.com") {
  render(<ChangeApprovalsPanel change={c} principal={principal} />);
}

describe("a change one person decides", () => {
  it("reads One person decides above today's approver classes, with no count", () => {
    panel(
      change({ approvers: [{ kind: "security" }, { kind: "unit_admin", unit_name: "Payments", name: "kai" }, { kind: "security" }] }),
    );
    expect(screen.getByText(CHANGE_APPROVALS.ONE_PERSON)).toBeInTheDocument();
    const classes = within(screen.getByTestId("change-approver-classes")).getAllByRole("listitem");
    expect(classes.map((li) => li.textContent)).toEqual([CHANGE_APPROVALS.TIER.security_admin, CHANGE_APPROVALS.TIER_UNIT("Payments")]);
    expect(screen.queryByTestId("change-approvals-progress")).toBeNull();
    expect(screen.queryByTestId("change-approvals-rule")).toBeNull();
  });

  it("draws no class list when the server named none", () => {
    panel(change());
    expect(screen.getByText(CHANGE_APPROVALS.ONE_PERSON)).toBeInTheDocument();
    expect(screen.queryByTestId("change-approver-classes")).toBeNull();
  });
});

describe("the count and the rule", () => {
  it("reads {have} of {required} approved to an approver and Waiting to the proposer", () => {
    panel(change({}, approvals({ viewer: { state: "can_approve" } })));
    expect(screen.getByTestId("change-approvals-progress")).toHaveTextContent("1 of 2 approved");
  });

  it("reads Waiting: {have} of {required} to the proposer, by viewer state and by principal", () => {
    const { unmount } = render(<ChangeApprovalsPanel change={change({}, approvals({ viewer: { state: "proposer" } }))} principal="" />);
    expect(screen.getByTestId("change-approvals-progress")).toHaveTextContent("Waiting: 1 of 2");
    unmount();
    panel(change({}, approvals()), "rory@example.com");
    expect(screen.getByTestId("change-approvals-progress")).toHaveTextContent("Waiting: 1 of 2");
  });

  it("states the security clause only when the server asks for it", () => {
    const { unmount } = render(<ChangeApprovalsPanel change={change({}, approvals({ required: 3 }))} principal="bo@example.com" />);
    expect(screen.getByTestId("change-approvals-rule")).toHaveTextContent(
      "Needs 3 approvals, at least one from a security admin. The proposer never counts.",
    );
    unmount();
    panel(change({}, approvals({ required: 3, require_security: false })));
    expect(screen.getByTestId("change-approvals-rule")).toHaveTextContent("Needs 3 approvals. The proposer never counts.");
  });

  it("keeps the security clause for a required 1 when the rule is on, and has none when it is off", () => {
    const { unmount } = render(<ChangeApprovalsPanel change={change({}, approvals({ required: 1 }))} principal="bo@example.com" />);
    expect(screen.getByTestId("change-approvals-rule")).toHaveTextContent("Needs 1 approval, from a security admin. The proposer never counts.");
    unmount();
    panel(change({}, approvals({ required: 1, require_security: false })));
    expect(screen.getByTestId("change-approvals-rule")).toHaveTextContent("Needs 1 approval. The proposer never counts.");
    expect(screen.getByTestId("change-approvals-rule").textContent).not.toContain("security");
  });
});

describe("the entries", () => {
  it("names who approved, when, and in which tier, with a reason as a quotation", () => {
    panel(
      change(
        {},
        approvals({
          entries: [
            { ...SAM, reason: "checked the hosts" },
            { approver: "kai@example.com", tier: "unit_admin", unit_name: "Payments", approved_at: aheadByHours(-3), counts: true },
            { approver: "ivy@example.com", tier: "super_admin", approved_at: aheadByHours(-4), counts: true },
          ],
        }),
      ),
    );
    const rows = screen.getAllByTestId("change-approval-entry");
    expect(rows[0]).toHaveTextContent("sam@example.com approved 1h ago · Security admin");
    expect(within(rows[0]).getByText("checked the hosts").tagName).toBe("Q");
    expect(rows[1]).toHaveTextContent("kai@example.com approved 3h ago · unit admin of Payments");
    expect(rows[2]).toHaveTextContent("ivy@example.com approved 4h ago · Admin");
  });

  it("strikes a dropped entry through, with the time only when the server has one", () => {
    const at = aheadByHours(-0.5);
    panel(
      change(
        {},
        approvals({
          entries: [
            { ...SAM, counts: false, dropped: "role_removed", dropped_at: at },
            { ...SAM, approver: "lee@example.com", approved_at: aheadByHours(-2), counts: false, dropped: "session_revoked" },
          ],
        }),
      ),
    );
    const [withTime, withoutTime] = screen.getAllByTestId("change-approval-dropped");
    expect(withTime.className).toContain("line-through");
    expect(withTime).toHaveTextContent(`sam@example.com's approval no longer counts: their security admin role was removed at ${shortTime(at).replace(/\s+/g, " ")}.`);
    expect(withoutTime).toHaveTextContent("lee@example.com's approval no longer counts: their sessions were revoked after they approved.");
    expect(withoutTime.textContent).not.toContain(" at ");
  });

  it.each([
    ["deactivated", "they were deactivated"],
    ["token_revoked", "the token they approved with was revoked"],
    ["session_revoked", "their sessions were revoked after they approved"],
    ["grant_removed", "their unit admin grant no longer covers this change"],
    ["cant_confirm", "their role can't be confirmed"],
  ] as const)("says why a %s approval no longer counts", (dropped, why) => {
    panel(change({}, approvals({ entries: [{ ...SAM, counts: false, dropped }] })));
    expect(screen.getByTestId("change-approval-dropped")).toHaveTextContent(`approval no longer counts: ${why}.`);
  });

  it("names the proposer as someone who does not count", () => {
    panel(change({}, approvals()));
    expect(screen.getByTestId("change-proposer-line")).toHaveTextContent("rory@example.com proposed this and doesn't count");
  });
});

describe("Can approve", () => {
  it("shows an entry's email when it has one and its principal when it does not, with the tier", () => {
    panel(
      change(
        {},
        approvals({
          eligible: [
            { principal: "p-kai", email: "kai@example.com", tier: "security_admin" },
            { principal: "p-max", tier: "super_admin" },
          ],
        }),
      ),
    );
    const list = screen.getByTestId("change-can-approve");
    expect(within(list).getByText("kai@example.com · Security admin")).toBeInTheDocument();
    expect(within(list).getByText("p-max · Admin")).toBeInTheDocument();
    expect(within(list).queryByText(/p-kai/)).toBeNull();
    expect(within(list).getByText(CHANGE_APPROVALS.NEVER_SIGNED_IN)).toBeInTheDocument();
  });

  it("says what the count leaves out", () => {
    panel(
      change(
        {},
        approvals({
          eligible: [{ principal: "p-kai", email: "kai@example.com", tier: "security_admin" }],
          uncountable: [{ why: "unconfirmed_people", count: 4 }, { why: "too_many_people" }],
        }),
      ),
    );
    const list = screen.getByTestId("change-can-approve");
    expect(within(list).getByText("and 4 people whose role can't be confirmed")).toBeInTheDocument();
    expect(within(list).getByText("and more people than Wardyn counts")).toBeInTheDocument();
  });

  it("draws nothing when the block has no eligible people and nothing uncountable", () => {
    panel(change({}, approvals()));
    expect(screen.queryByTestId("change-can-approve")).toBeNull();
  });
});

describe("whether the change can reach its count", () => {
  const stuck = approvals({ reach: "cant_reach", still_needed: 1, remaining_eligible: 0 });

  it("count: names the numbers, and the withdraw clause shows to the proposer only", () => {
    const { unmount } = render(<ChangeApprovalsPanel change={change({}, { ...stuck, cant_reach_reason: "count" })} principal="rory@example.com" />);
    const mine = screen.getByTestId("change-cant-reach");
    expect(mine).toHaveTextContent("This change can't reach 2 approvals: it still needs 1, and only 0 more can approve it.");
    expect(mine.textContent).toMatch(/It expires in 3d unless someone is given the right to approve it, or you withdraw it\.$/);
    unmount();
    panel(change({}, { ...stuck, cant_reach_reason: "count" }));
    const other = screen.getByTestId("change-cant-reach");
    expect(other.textContent).toMatch(/It expires in 3d unless someone is given the right to approve it\.$/);
    expect(other.textContent).not.toContain("withdraw");
  });

  it("security: names the missing security admin, with no number, even when enough people remain", () => {
    panel(change({}, { ...stuck, still_needed: 1, remaining_eligible: 3, cant_reach_reason: "security" }), "rory@example.com");
    const sentence = screen.getByTestId("change-cant-reach");
    expect(sentence).toHaveTextContent("This change can't be approved: it needs a security admin, and no one who can still approve it is one.");
    expect(sentence.textContent).not.toContain("only 3 more");
    expect(sentence.textContent).toMatch(/, or you withdraw it\.$/);
  });

  it("an absent reason reads as count only when fewer people remain than are needed, and as security otherwise", () => {
    const { unmount } = render(<ChangeApprovalsPanel change={change({}, { ...stuck, still_needed: 2, remaining_eligible: 1 })} principal="bo@example.com" />);
    expect(screen.getByTestId("change-cant-reach")).toHaveAttribute("data-reason", "count");
    unmount();
    render(<ChangeApprovalsPanel change={change({}, { ...stuck, still_needed: 1, remaining_eligible: 1 })} principal="bo@example.com" />);
    expect(screen.getByTestId("change-cant-reach")).toHaveAttribute("data-reason", "security");
  });

  it("an unrecognised reason is treated as absent", () => {
    panel(change({}, { ...stuck, still_needed: 2, remaining_eligible: 1, cant_reach_reason: "other" as unknown as "count" }));
    expect(screen.getByTestId("change-cant-reach")).toHaveAttribute("data-reason", "count");
  });

  it("says Wardyn can't confirm the change can reach its count when the server cannot", () => {
    panel(change({}, approvals({ required: 3, reach: "cant_confirm" })));
    expect(screen.getByTestId("change-cant-confirm")).toHaveTextContent(
      "Wardyn can't confirm this change can reach 3 approvals: some people who could approve it can't be counted.",
    );
    expect(screen.queryByTestId("change-cant-reach")).toBeNull();
  });

  it("says nothing when the change can reach its count", () => {
    panel(change({}, approvals({ reach: "ok" })));
    expect(screen.queryByTestId("change-cant-reach")).toBeNull();
    expect(screen.queryByTestId("change-cant-confirm")).toBeNull();
  });
});

describe("the viewer's own standing in the panel", () => {
  it("cannot_be_last says the last approval must come from a security admin", () => {
    panel(change({}, approvals({ viewer: { state: "cannot_be_last" } })));
    expect(screen.getByTestId("change-cannot-be-last")).toHaveTextContent(CHANGE_APPROVALS.CANNOT_BE_LAST);
  });

  it("says nothing of it for any other state", () => {
    panel(change({}, approvals({ viewer: { state: "can_approve" } })));
    expect(screen.queryByTestId("change-cannot-be-last")).toBeNull();
  });
});
