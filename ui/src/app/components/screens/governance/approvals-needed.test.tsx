/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Approvals needed tab. Expected strings come from approvals-needed-copy.ts and CHANGES, so these fail the
// moment a rendered string stops coming from the copy. Pinned here because they are decisions: with no minimum set
// the table is read-only and never claims every change needs one approval; a save the server holds is
// "submitted", never "saved"; the server's own sentence on a refusal is shown as written.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getCountsMock = vi.fn();
const putCountsMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      getApprovalCounts: (...a: unknown[]) => getCountsMock(...a),
      putApprovalCounts: (...a: unknown[]) => putCountsMock(...a),
    },
  };
});

import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { APPROVALS_NEEDED as COPY } from "../../../lib/approvals-needed-copy";
import { CHANGES } from "../../../lib/governance-copy";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange } from "../../../lib/types";
import type { ApprovalCounts, ApprovalCountsKind } from "../../../lib/types/approval-counts";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ApprovalsNeededTab } from "./approvals-needed";
import { ChangesTab } from "./changes";

const ENV = "WARDYN_GOVERNANCE_MIN_APPROVALS";
const MIN_ZERO_TEXT = COPY.MIN_ZERO.replace(COPY.ENV_SLOT, ENV);

function kindRow(kind: string, over: Partial<ApprovalCountsKind> = {}): ApprovalCountsKind {
  return {
    kind,
    own: true,
    required: 2,
    require_security: false,
    approvers: "security",
    pool: { status: "ok", count: 5 },
    ...over,
  };
}

function counts(over: Partial<ApprovalCounts> = {}): ApprovalCounts {
  return {
    minimum: 2,
    highest: 3,
    default: { required: 2, require_security: false },
    kinds: [
      kindRow("role_mapping", { required: 3, approvers: "super_admin" }),
      kindRow("governance_envelope", { require_security: true }),
      kindRow("egress_baseline", { own: false }),
    ],
    ...over,
  };
}

function held(): GovernanceChange {
  return {
    id: "held1",
    target_kind: "governance_approval_counts",
    op: "set",
    target_key: "approval-counts",
    state: "pending",
    proposed_by: "ana@example.com",
    proposed_at: aheadByHours(-1),
    expires_at: aheadByHours(23),
    diff: { changed: [] },
  };
}

async function open(c: ApprovalCounts, onSubmitted = vi.fn()) {
  getCountsMock.mockResolvedValue(c);
  render(
    <MemoryRouter>
      <OperatorProvider operator={false}>
        <ApprovalsNeededTab onSubmitted={onSubmitted} />
      </OperatorProvider>
    </MemoryRouter>,
  );
  await screen.findByText(COPY.MIN_LABEL);
  return { onSubmitted };
}

const rowFor = (label: string) => screen.getAllByRole("row").find((r) => within(r).queryByText(label))!;
const box = (label: string) => screen.getByRole("spinbutton", { name: COPY.ROW_FIELD(label, COPY.COL_APPROVALS) });

beforeEach(() => {
  getCountsMock.mockReset();
  putCountsMock.mockReset();
});

describe("the Approvals needed tab", () => {
  it("shows the title and lede, then the deploy-time minimum in a locked box", async () => {
    await open(counts());
    expect(screen.getByRole("heading", { name: COPY.TITLE })).toBeInTheDocument();
    expect(COPY.TITLE).toBe(CHANGES.KIND.governance_approval_counts);
    expect(screen.getByText(COPY.LEAD)).toBeInTheDocument();
    expect(screen.getByText(COPY.MIN_HINT)).toBeInTheDocument();
    const min = screen.getByRole("spinbutton", { name: COPY.MIN_LABEL });
    expect(min).toHaveValue(2);
    expect(min).toBeDisabled();
    expect(getCountsMock).toHaveBeenCalledTimes(1);
  });

  it("lists the own rows, then Every other kind, and leaves a kind on the default off the table", async () => {
    await open(counts());
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(3);
    expect(within(rows[0]).getByText(CHANGES.KIND.role_mapping)).toBeInTheDocument();
    expect(within(rows[1]).getByText(CHANGES.KIND.governance_envelope)).toBeInTheDocument();
    expect(within(rows[2]).getByText(COPY.EVERY_OTHER_KIND)).toBeInTheDocument();
    expect(screen.queryByText(CHANGES.KIND.egress_baseline)).toBeNull();
    expect(box(CHANGES.KIND.role_mapping)).toHaveValue(3);
    expect(within(rows[2]).getByText(COPY.DEFAULT_COUNT)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: COPY.ROW_FIELD(CHANGES.KIND.governance_envelope, COPY.COL_SECURITY) })).toBeChecked();
  });

  it("gives the number boxes the bounds of the minimum and nine", async () => {
    await open(counts({ minimum: 2 }));
    expect(box(CHANGES.KIND.role_mapping)).toHaveAttribute("min", "2");
    expect(box(CHANGES.KIND.role_mapping)).toHaveAttribute("max", "9");
  });

  it("names who can approve each kind", async () => {
    await open(
      counts({
        kinds: [
          kindRow("role_mapping", { approvers: "super_admin" }),
          kindRow("governance_envelope", { approvers: "security" }),
          kindRow("governance_unit", { approvers: "security_or_unit_admin" }),
        ],
      }),
    );
    expect(within(rowFor(CHANGES.KIND.role_mapping)).getByText(COPY.WHO.super_admin)).toBeInTheDocument();
    expect(within(rowFor(CHANGES.KIND.governance_envelope)).getByText(COPY.WHO.security)).toBeInTheDocument();
    expect(within(rowFor(CHANGES.KIND.governance_unit)).getByText(COPY.WHO.security_or_unit_admin)).toBeInTheDocument();
  });

  it("says what is under the table and the count a held edit needs", async () => {
    await open(counts({ highest: 4 }));
    expect(screen.getByText(COPY.NEVER_SIGNED_IN)).toBeInTheDocument();
    expect(screen.getByText(COPY.NOTE_HELD(4))).toBeInTheDocument();
    expect(screen.getByText(COPY.NOTE_REQUESTS)).toBeInTheDocument();
  });

  it("builds no way to give a kind its own count: no select, no remove control", async () => {
    await open(counts());
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button", { name: /remove/i })).toBeNull();
  });
});

describe("each pool status", () => {
  it("ok shows the count, and the smallest unit for a unit kind", async () => {
    await open(
      counts({
        kinds: [
          kindRow("governance_envelope", { pool: { status: "ok", count: 5 } }),
          kindRow("governance_unit", { approvers: "security_or_unit_admin", pool: { status: "ok", count: 3, unit_name: "Payments" } }),
        ],
      }),
    );
    expect(within(rowFor(CHANGES.KIND.governance_envelope)).getByText("5")).toBeInTheDocument();
    expect(within(rowFor(CHANGES.KIND.governance_unit)).getByText(COPY.IN_SMALLEST_UNIT(3, "Payments"))).toBeInTheDocument();
  });

  it("exact says the change passes only when the proposer is not one of them", async () => {
    await open(counts({ kinds: [kindRow("role_mapping", { required: 3, pool: { status: "exact", count: 3 } })] }));
    expect(within(rowFor(CHANGES.KIND.role_mapping)).getByText(COPY.EXACT(3))).toBeInTheDocument();
    expect(COPY.EXACT(3)).toContain("only 2 others");
  });

  it("short says it can't be met, with the people who can approve and the count asked for", async () => {
    await open(counts({ kinds: [kindRow("role_mapping", { required: 3, pool: { status: "short", count: 2 } })] }));
    expect(within(rowFor(CHANGES.KIND.role_mapping)).getByText(COPY.SHORT(2, CHANGES.KIND.role_mapping, 3))).toBeInTheDocument();
  });

  it("cant_confirm says so, and why for each reason", async () => {
    await open(
      counts({
        kinds: [
          kindRow("role_mapping", { pool: { status: "cant_confirm", count: 0, why: "unconfirmed_people" } }),
          kindRow("governance_envelope", { pool: { status: "cant_confirm", count: 0, why: "too_many_people" } }),
          kindRow("governance_unit", { pool: { status: "cant_confirm", count: 0 } }),
        ],
      }),
    );
    const a = within(rowFor(CHANGES.KIND.role_mapping));
    expect(a.getByText(COPY.CANT_CONFIRM)).toBeInTheDocument();
    expect(a.getByText(COPY.CANT_CONFIRM_UNCONFIRMED)).toBeInTheDocument();
    expect(within(rowFor(CHANGES.KIND.governance_envelope)).getByText(COPY.CANT_CONFIRM_TOO_MANY)).toBeInTheDocument();
    const bare = within(rowFor(CHANGES.KIND.governance_unit));
    expect(bare.getByText(COPY.CANT_CONFIRM)).toBeInTheDocument();
    expect(bare.queryByText(COPY.CANT_CONFIRM_UNCONFIRMED)).toBeNull();
    expect(bare.queryByText(COPY.CANT_CONFIRM_TOO_MANY)).toBeNull();
  });
});

describe("with no minimum set", () => {
  const unset = () => counts({ minimum: 0, highest: 1, default: { required: 1, require_security: false }, kinds: [kindRow("role_mapping", { required: 1 })] });

  it("is read-only, with no Save", async () => {
    await open(unset());
    expect(box(CHANGES.KIND.role_mapping)).toBeDisabled();
    expect(box(COPY.EVERY_OTHER_KIND)).toBeDisabled();
    expect(screen.getByRole("switch", { name: COPY.ROW_FIELD(COPY.EVERY_OTHER_KIND, COPY.COL_SECURITY) })).toBeDisabled();
    expect(screen.queryByRole("button", { name: COPY.SAVE })).toBeNull();
    expect(screen.getByRole("spinbutton", { name: COPY.MIN_LABEL })).toHaveValue(0);
  });

  it("says exactly what the copy says, the variable's name in mono, and never that every change needs one", async () => {
    await open(unset());
    const note = screen.getByTestId("approvals-minimum-zero");
    expect(note).toHaveTextContent(MIN_ZERO_TEXT);
    expect(note.querySelector("code")).toHaveTextContent(ENV);
    expect(note.textContent).toContain("Pending changes keep their existing approval requirements.");
    expect(note.textContent).not.toMatch(/every change needs one/i);
    expect(COPY.MIN_ZERO).not.toMatch(/every change needs one/i);
  });

  it("says pending changes keep their requirement, beside a pending change that needs three", async () => {
    getCountsMock.mockResolvedValue(unset());
    const pending: GovernanceChange = {
      ...held(),
      id: "p3",
      target_kind: "governance_unit",
      target_key: "team-a",
      approvals: { required: 3, have: 0, require_security: false, viewer: { state: "can_approve" } },
    };
    render(
      <MemoryRouter>
        <OperatorProvider operator={false} principal="bo@example.com">
          <ApprovalsNeededTab />
          <ChangesTab changes={[pending]} loadError={null} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText(CHANGES.APPROVED(0, 3))).toBeInTheDocument();
    expect(screen.getByTestId("approvals-minimum-zero")).toHaveTextContent("Pending changes keep their existing approval requirements.");
    expect(screen.queryByText(/every change needs one/i)).toBeNull();
  });
});

describe("Save", () => {
  it("sends the default and the own rows only, and reloads after a 200", async () => {
    await open(counts());
    getCountsMock.mockResolvedValue(counts({ default: { required: 2, require_security: false } }));
    putCountsMock.mockResolvedValue(counts());
    const save = screen.getByRole("button", { name: COPY.SAVE });
    expect(save).toBeDisabled();
    await userEvent.clear(box(CHANGES.KIND.role_mapping));
    await userEvent.type(box(CHANGES.KIND.role_mapping), "4");
    await userEvent.click(screen.getByRole("switch", { name: COPY.ROW_FIELD(COPY.EVERY_OTHER_KIND, COPY.COL_SECURITY) }));
    await userEvent.click(save);
    expect(putCountsMock).toHaveBeenCalledWith({
      default: { required: 2, require_security: true },
      kinds: [
        { kind: "role_mapping", required: 4, require_security: false },
        { kind: "governance_envelope", required: 2, require_security: true },
      ],
    });
    await vi.waitFor(() => expect(getCountsMock).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("holds Save while a count is outside the number box's bounds", async () => {
    await open(counts({ minimum: 2 }));
    await userEvent.clear(box(CHANGES.KIND.role_mapping));
    await userEvent.type(box(CHANGES.KIND.role_mapping), "1");
    expect(screen.getByRole("button", { name: COPY.SAVE })).toBeDisabled();
    await userEvent.clear(box(CHANGES.KIND.role_mapping));
    await userEvent.type(box(CHANGES.KIND.role_mapping), "9");
    expect(screen.getByRole("button", { name: COPY.SAVE })).toBeEnabled();
  });

  it("a held save shows the submitted note, is not read back as applied, and calls onSubmitted", async () => {
    const { onSubmitted } = await open(counts());
    putCountsMock.mockRejectedValue(new PendingChangeError(held()));
    await userEvent.clear(box(CHANGES.KIND.role_mapping));
    await userEvent.type(box(CHANGES.KIND.role_mapping), "4");
    await userEvent.click(screen.getByRole("button", { name: COPY.SAVE }));
    const note = await screen.findByRole("status");
    expect(within(note).getByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(getCountsMock).toHaveBeenCalledTimes(1);
    expect(onSubmitted).toHaveBeenCalledTimes(1);
    expect(box(CHANGES.KIND.role_mapping)).toHaveValue(4);
    expect(screen.getByRole("button", { name: COPY.SAVE })).toBeDisabled();
  });

  it("a 422 shows the server's sentence under a title, then what to do next", async () => {
    await open(counts());
    putCountsMock.mockRejectedValue(new HttpError(422, "role_mapping needs 9 and only 2 people can approve it"));
    await userEvent.clear(box(CHANGES.KIND.role_mapping));
    await userEvent.type(box(CHANGES.KIND.role_mapping), "9");
    await userEvent.click(screen.getByRole("button", { name: COPY.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByRole("heading", { name: COPY.REFUSED_TITLE })).toBeInTheDocument();
    expect(within(alert).getByText("role_mapping needs 9 and only 2 people can approve it")).toBeInTheDocument();
    expect(within(alert).getByText(COPY.REFUSED_NEXT)).toBeInTheDocument();
  });

  it("any other refusal shows the server's sentence as written, without the next step", async () => {
    await open(counts());
    putCountsMock.mockRejectedValue(new HttpError(403, "not allowed here"));
    await userEvent.clear(box(CHANGES.KIND.role_mapping));
    await userEvent.type(box(CHANGES.KIND.role_mapping), "4");
    await userEvent.click(screen.getByRole("button", { name: COPY.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("not allowed here")).toBeInTheDocument();
    expect(within(alert).queryByText(COPY.REFUSED_NEXT)).toBeNull();
  });
});

describe("when the counts cannot be read", () => {
  it("says so and offers a retry that reads again", async () => {
    getCountsMock.mockRejectedValueOnce(new Error("down"));
    render(
      <MemoryRouter>
        <OperatorProvider operator={false}>
          <ApprovalsNeededTab />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText(COPY.FETCH_FAILED_TITLE)).toBeInTheDocument();
    getCountsMock.mockResolvedValue(counts());
    await userEvent.click(screen.getByRole("button"));
    expect(await screen.findByText(COPY.MIN_LABEL)).toBeInTheDocument();
  });
});
