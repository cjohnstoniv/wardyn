/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Changes tab and its drawer (0.8.6 four-eyes, mock packet M3). Expected strings come from CHANGES, so
// these fail the moment a rendered string stops coming from the approved canon. Pinned here because they are
// decisions: the diff is the server's, Approve is disabled on the viewer's own proposal, the server's own
// refusal sentence is shown verbatim, and a 202 at a write site is "submitted", never "saved".
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const approveMock = vi.fn();
const rejectMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      approveChange: (...a: unknown[]) => approveMock(...a),
      rejectChange: (...a: unknown[]) => rejectMock(...a),
    },
  };
});

import { HttpError } from "../../../lib/api/core";
import { CHANGES } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { aheadByHours } from "../../../lib/test-clock";
import { ChangesTab } from "./changes";
import { SubmittedNote } from "./submitted-note";

function change(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_profile",
    op: "update",
    target_key: "team-a",
    state: "pending",
    proposed_by: "ana@example.com",
    proposed_at: aheadByHours(-2),
    expires_at: aheadByHours(20),
    diff: {
      before: { ceiling: { allowed_domains: ["*.corp", "git.x"] }, name: "team-a" },
      after: { ceiling: { allowed_domains: ["*.corp"] }, name: "team-a" },
      changed: ["ceiling.allowed_domains"],
    },
    ...over,
  };
}

function renderTab(changes: GovernanceChange[], principal = "bo@example.com", onChanged = vi.fn()) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal={principal}>
        <ChangesTab changes={changes} loadError={null} onChanged={onChanged} />
      </OperatorProvider>
    </MemoryRouter>,
  );
  return { onChanged };
}

beforeEach(() => {
  approveMock.mockReset();
  rejectMock.mockReset();
  vi.mocked(toast.success).mockClear();
});

describe("the Changes tab", () => {
  it("lists a pending change as Kind · Op over its target, proposer and times", () => {
    renderTab([change()]);
    expect(screen.getByRole("columnheader", { name: CHANGES.COL_TARGET })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Profile · Edit" })).toBeInTheDocument();
    expect(screen.getByText("team-a")).toBeInTheDocument();
    expect(screen.getByText("ana@example.com")).toBeInTheDocument();
    expect(screen.getByText("2h ago")).toBeInTheDocument();
    expect(screen.getByText("in 20h")).toBeInTheDocument();
  });

  it("names a kind and an op this console predates by their raw names", () => {
    renderTab([change({ target_kind: "future_kind", op: "merge" })]);
    expect(screen.getByRole("button", { name: "future_kind · merge" })).toBeInTheDocument();
  });

  it("names a profile by the name in the server's diff, since its key is an id", () => {
    renderTab([change({ target_key: "7b1f0c52-0000-4000-8000-000000000001" })]);
    expect(screen.getByText("team-a")).toBeInTheDocument();
    expect(screen.queryByText("7b1f0c52-0000-4000-8000-000000000001")).toBeNull();
  });

  it("keeps the key as the target for a kind whose key is already natural", () => {
    renderTab([change({ target_kind: "governance_assignment", op: "upsert", target_key: "group:eng", diff: { changed: [] } })]);
    expect(screen.getByText("group:eng")).toBeInTheDocument();
  });

  it("with nothing pending shows the empty state, with no action", () => {
    renderTab([]);
    expect(screen.getByText(CHANGES.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(CHANGES.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });
});

describe("the change drawer", () => {
  it("renders the server's diff as Field / Before / After, and the full before and after behind a disclosure", async () => {
    renderTab([change()]);
    await userEvent.click(screen.getByRole("button", { name: "Profile · Edit" }));
    const dialog = await screen.findByRole("dialog");

    expect(within(dialog).getByText(CHANGES.META("ana@example.com", "2h ago", "in 20h"))).toBeInTheDocument();
    expect(within(dialog).getByText("ceiling.allowed_domains")).toBeInTheDocument();
    expect(within(dialog).getByText('["*.corp","git.x"]')).toBeInTheDocument();
    expect(within(dialog).getByText('["*.corp"]')).toBeInTheDocument();
    expect(within(dialog).getByText(CHANGES.DIFF_FULL)).toBeInTheDocument();
    expect(within(dialog).getByText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(within(dialog).getByText(CHANGES.REASON_HINT)).toBeInTheDocument();
  });

  it("shows an absent side as Not set", async () => {
    renderTab([
      change({
        op: "create",
        diff: { after: { name: "team-a" }, changed: ["name"] },
      }),
    ]);
    await userEvent.click(screen.getByRole("button", { name: "Profile · New" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getAllByText(CHANGES.DIFF_UNSET).length).toBeGreaterThan(0);
  });

  it("renders an assignment's embedded profile summary from the diff, and looks nothing up", async () => {
    renderTab([
      change({
        target_kind: "governance_assignment",
        op: "upsert",
        diff: {
          after: {
            subject_type: "group",
            subject: "g",
            profile_id: "p9",
            priority: 0,
            profile: { id: "p9", name: "Embedded profile", ceiling: { allowed_domains: ["a.example"] }, limits: {} },
          },
          changed: ["profile_id"],
        },
      }),
    ]);
    await userEvent.click(screen.getByRole("button", { name: "Assignment · Set" }));
    const dialog = await screen.findByRole("dialog");
    const section = within(dialog).getByTestId("change-assigned-profile");
    expect(within(section).getByText(CHANGES.ASSIGNED_PROFILE)).toBeInTheDocument();
    expect(within(section).getByText("Embedded profile")).toBeInTheDocument();
    expect(section.textContent).toContain("a.example");
  });

  it("disables Approve on the viewer's own proposal, says why, and still lets them Reject", async () => {
    renderTab([change()], "ana@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Profile · Edit" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("button", { name: CHANGES.APPROVE })).toBeDisabled();
    expect(within(dialog).getByText(CHANGES.OWN_NOTE)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: CHANGES.REJECT })).toBeEnabled();
  });

  it("approves another person's change, toasts, and asks for the list again", async () => {
    approveMock.mockResolvedValue(change({ state: "applied" }));
    const { onChanged } = renderTab([change()]);
    await userEvent.click(screen.getByRole("button", { name: "Profile · Edit" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryByText(CHANGES.OWN_NOTE)).toBeNull();
    await userEvent.click(within(dialog).getByRole("button", { name: CHANGES.APPROVE }));
    expect(approveMock).toHaveBeenCalledWith("c1");
    expect(toast.success).toHaveBeenCalledWith(CHANGES.TOAST_APPROVED);
    expect(onChanged).toHaveBeenCalled();
  });

  it("rejects with the typed reason", async () => {
    rejectMock.mockResolvedValue(change({ state: "rejected" }));
    renderTab([change()]);
    await userEvent.click(screen.getByRole("button", { name: "Profile · Edit" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(CHANGES.REASON_LABEL), "not now");
    await userEvent.click(within(dialog).getByRole("button", { name: CHANGES.REJECT }));
    expect(rejectMock).toHaveBeenCalledWith("c1", "not now");
    expect(toast.success).toHaveBeenCalledWith(CHANGES.TOAST_REJECTED);
  });

  it.each([403, 409])("shows the server's own sentence on a %i, under the frozen heading", async (status) => {
    approveMock.mockRejectedValue(new HttpError(status, "the change you reviewed is no longer current"));
    renderTab([change()]);
    await userEvent.click(screen.getByRole("button", { name: "Profile · Edit" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: CHANGES.APPROVE }));
    const alert = await within(dialog).findByRole("alert");
    expect(within(alert).getByText(CHANGES.DECIDE_REFUSED_TITLE)).toBeInTheDocument();
    expect(within(alert).getByText("the change you reviewed is no longer current")).toBeInTheDocument();
    expect(toast.success).not.toHaveBeenCalled();
  });
});

describe("the submitted-for-approval note", () => {
  it("says nothing has changed yet, and links to the Changes tab", () => {
    render(
      <MemoryRouter>
        <SubmittedNote />
      </MemoryRouter>,
    );
    const note = screen.getByRole("status");
    expect(within(note).getByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(within(note).getByText(CHANGES.SUBMITTED_BODY)).toBeInTheDocument();
    expect(within(note).getByRole("link", { name: CHANGES.SUBMITTED_LINK })).toHaveAttribute(
      "href",
      "/admin/governance?tab=changes",
    );
  });

  it("renders without a router, minus the link", () => {
    render(<SubmittedNote />);
    expect(screen.getByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("link")).toBeNull();
  });
});
