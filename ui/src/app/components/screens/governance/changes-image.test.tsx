/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// An image request on the Changes tab: its row (label, line, target), its page (five rows, the full digest), the
// requester's view, and a stale answer to Approve. Strings are the approved copy's, spelled out.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: { get: vi.fn(), members: vi.fn() },
}));

const getChangeMock = vi.fn();
const approveMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      getChange: (...a: unknown[]) => getChangeMock(...a),
      approveChange: (...a: unknown[]) => approveMock(...a),
    },
  };
});

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ChangeReviewPage } from "./change-review-page";
import { ChangesTab } from "./changes";

const HEX = "4f2a91c3" + "0".repeat(56);
const REF = `ghcr.io/acme/build-tools@sha256:${HEX}`;

function image(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "i1",
    target_kind: "governance_image_request",
    op: "create",
    target_key: `dana.reyes|sha256:${HEX}`,
    state: "pending",
    proposed_by: "dana.reyes",
    proposed_at: aheadByHours(-1),
    expires_at: aheadByHours(23),
    diff: { after: { image: REF, unit_id: "u1", unit_name: "Payments", subject: "dana.reyes" }, changed: ["image"] },
    note: { reason: "The nightly build needs the new toolchain", ticket: "INC-2210" },
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
  approveMock.mockReset();
});

describe("the page title", () => {
  // jsdom measures no layout; this pins the structure. Each part of the title is a flex item of its own that may
  // shrink and break anywhere, since min-w-0 on the title does not reach an anonymous text item.
  it("makes every part of the title shrinkable and breakable", async () => {
    renderPage(image());
    const title = await screen.findByRole("heading", { level: 1 });
    expect(title.children.length).toBeGreaterThanOrEqual(2);
    for (const part of Array.from(title.children)) {
      expect(part.className).toContain("min-w-0");
      expect(part.className).toContain("[overflow-wrap:anywhere]");
    }
  });
});

describe("an image request in the list", () => {
  it("reads as an image request: label, its line with the ticket, and the person → image target in mono", () => {
    renderTab([image()]);
    const row = screen.getByRole("row", { name: /Image · New/ });
    expect(within(row).getByRole("button", { name: "Image · New" })).toBeInTheDocument();
    expect(row.querySelector("p")).toHaveTextContent("One person, this digest only · INC-2210");
    const target = within(row).getByText("dana.reyes → build-tools@sha256:4f2a91c3…");
    expect(target).toHaveClass("font-mono");
    expect(row).not.toHaveTextContent("The nightly build needs the new toolchain");
  });

  it("leaves the ticket off when there is none", () => {
    renderTab([image({ note: undefined })]);
    expect(screen.getByRole("row", { name: /Image · New/ }).querySelector("p")).toHaveTextContent(/^One person, this digest only$/);
  });

  it("falls back to the generic row when a field is missing", () => {
    renderTab([image({ diff: { after: { subject: "dana.reyes" }, changed: [] } })]);
    const row = screen.getByRole("row", { name: /Image · New/ });
    expect(row).not.toHaveTextContent("this digest only");
    expect(within(row).getByText(`dana.reyes|sha256:${HEX}`)).toBeInTheDocument();
  });
});

describe("an image request's page", () => {
  it("shows five rows with the full digest, the target in the title, and keeps the full diff", async () => {
    renderPage(image());
    expect(await screen.findByRole("heading", { level: 1, name: /Image · New\s*dana\.reyes → build-tools@sha256:4f2a91c3…/ })).toBeInTheDocument();
    const rows = screen.getAllByRole("row").map((r) => r.textContent);
    expect(rows).toEqual([
      "Persondana.reyes",
      "Imageghcr.io/acme/build-tools",
      `Digestsha256:${HEX}`,
      "Asked underPayments, their default profile",
      "Approving writesAn allow grant of exactly this image for this person. It holds in any profile they run in. The digest rule, a deny of the image and image admission still apply at every launch.",
    ]);
    expect(screen.getByText(`sha256:${HEX}`)).toHaveClass("break-all");
    expect(screen.getByText("Show full before and after")).toBeInTheDocument();
    expect(screen.getByTestId("change-note")).toHaveTextContent("The nightly build needs the new toolchain");
    expect(screen.getByTestId("change-note")).toHaveTextContent("INC-2210");
  });

  it("someone else's request keeps Reject and Approve and the decider's reason", async () => {
    renderPage(image());
    expect(await screen.findByLabelText("Reason for your decision (optional)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reject" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "Withdraw request" })).toBeNull();
  });

  it("the requester sees Withdraw request, the request note, no reason field, and Approve off", async () => {
    renderPage(image(), "dana.reyes");
    expect(await screen.findByRole("button", { name: "Withdraw request" })).toBeEnabled();
    expect(screen.getByText("You asked for this, so someone else must approve it. You can withdraw it.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Reason for your decision (optional)")).toBeNull();
    expect(screen.queryByRole("button", { name: "Reject" })).toBeNull();
    expect(screen.getByRole("button", { name: "Approve" })).toBeDisabled();
  });

  it("Approve answered stale shows the server's sentence under the refusal heading", async () => {
    const sentence =
      "the target, or the deployment default, has changed since this change was proposed: it was not applied and is now stale; propose it again";
    approveMock.mockRejectedValue(new HttpError(409, sentence, "governance_change_stale"));
    renderPage(image());
    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByText("Couldn't decide this change")).toBeInTheDocument();
    expect(screen.getByText(sentence)).toHaveClass("font-mono");
    expect(approveMock).toHaveBeenCalledWith("i1", undefined);
  });
});
