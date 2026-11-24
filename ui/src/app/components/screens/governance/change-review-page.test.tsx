/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A change's own page: the tabs at 1024 px and the stacked order with the pinned bar at 390 px, what it loads and
// when, how each decision ends, which changes read as stale, the request kinds, and the four mount points the review
// assistant fills. The fixtures follow the server's approvals block (internal/api/governance_change_approvals.go).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { toast } from "sonner";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), warning: vi.fn() } }));

const getChangeMock = vi.fn();
const impactMock = vi.fn();
const approveMock = vi.fn();
const rejectMock = vi.fn();
const takeBackMock = vi.fn();
const noteMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      getChange: (...a: unknown[]) => getChangeMock(...a),
      getChangeImpact: (...a: unknown[]) => impactMock(...a),
      approveChange: (...a: unknown[]) => approveMock(...a),
      rejectChange: (...a: unknown[]) => rejectMock(...a),
      takeBackApproval: (...a: unknown[]) => takeBackMock(...a),
      noteChange: (...a: unknown[]) => noteMock(...a),
    },
  };
});

// The four mount points, replaced by markers that record the change they were given.
const slots = vi.hoisted(() => ({ props: {} as Record<string, unknown>, column: false }));
vi.mock("./change-review-slots", async () => {
  const React = await import("react");
  const slot = (name: string) => (p: { change: unknown }) => {
    slots.props[name] = p.change;
    return React.createElement("span", { "data-testid": `slot-${name}` });
  };
  return {
    get REVIEW_ASSISTANT_COLUMN() {
      return slots.column;
    },
    ReviewAssistantColumn: slot("column"),
    ReviewAssistantSheetButton: slot("sheet"),
    ReviewReplaySlot: slot("replay"),
    ReviewHeaderChip: slot("chip"),
  };
});

import { HttpError } from "../../../lib/api/core";
import { CHANGE_APPROVALS, CHANGE_REVIEW } from "../../../lib/change-review-copy";
import { CHANGES, GOVERNANCE } from "../../../lib/governance-copy";
import { aheadByHours } from "../../../lib/test-clock";
import type { ChangeImpact, GovernanceChange, GovernanceChangeApprovals } from "../../../lib/types";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { PAGE_NOT_FOUND } from "../../wardyn/copy/refusal";
import { OperatorProvider } from "../../wardyn/operator-context";
import { setField } from "../../../../test/set-field";
import { ChangeReviewPage } from "./change-review-page";

const SAMPLE_APPROVALS: GovernanceChangeApprovals = {
  required: 2,
  require_security: true,
  have: 1,
  entries: [{ approver: "sam@example.com", tier: "security_admin", approved_at: aheadByHours(-1), counts: true }],
  eligible: [{ principal: "p-kai", email: "kai@example.com", tier: "security_admin" }],
  uncountable: [{ why: "unconfirmed_people", count: 4 }],
  reach: "ok",
  still_needed: 1,
  remaining_eligible: 3,
  proposer_active: true,
  reject_reason_required: true,
  viewer: { state: "can_approve", last: true },
};

const WIDENING = {
  verdict: "widens" as const,
  widens: [{ field: "allowed_domains", entries: ["settle.partner-a.example"] }],
  risk: { before: { overall: "low" as const, items: [] }, after: { overall: "medium" as const, items: [] } },
};

function change(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_unit",
    op: "update",
    target_key: "team-a",
    state: "pending",
    proposed_by: "rory@example.com",
    proposed_at: aheadByHours(-2),
    expires_at: aheadByHours(20),
    diff: {
      before: { ceiling: { allowed_domains: ["*.corp", "git.x"] }, name: "team-a" },
      after: { ceiling: { allowed_domains: ["*.corp"] }, name: "team-a" },
      changed: ["ceiling.allowed_domains"],
      review: WIDENING,
    },
    approvals: SAMPLE_APPROVALS,
    ...over,
  };
}

const TREE: ChangeImpact = { computed: "tree", as_of: aheadByHours(0), people: [], live_runs_total: 3 };

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}

// The page lays out on its own width, which jsdom does not measure: a page 800 px wide gets tabs, one 390 px wide is
// stacked, whatever the window is.
let widthSpy: ReturnType<typeof vi.spyOn> | undefined;
function stubWidth(wide: boolean) {
  widthSpy?.mockRestore();
  widthSpy = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(wide ? 800 : 390);
}

function renderPage(c: GovernanceChange | Error, principal = "bo@example.com", path = `/admin/governance/changes/${(c as GovernanceChange).id}`) {
  if (c instanceof Error) getChangeMock.mockRejectedValue(c);
  else getChangeMock.mockResolvedValue(c);
  return render(
    <MemoryRouter initialEntries={[path]}>
      <OperatorProvider operator={false} principal={principal}>
        <Routes>
          <Route path="/admin/governance/changes/:id" element={<ChangeReviewPage />} />
          <Route path="/admin/governance" element={<Where />} />
          <Route path="*" element={<p>{PAGE_NOT_FOUND.TITLE}</p>} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const follows = (a: Element, b: Element) => Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
const approveButton = () => screen.getByRole("button", { name: CHANGES.APPROVE });
const rejectButton = () => screen.getByRole("button", { name: CHANGES.REJECT });

beforeEach(() => {
  for (const m of [getChangeMock, impactMock, approveMock, rejectMock, takeBackMock, noteMock]) m.mockReset();
  impactMock.mockResolvedValue(TREE);
  slots.props = {};
  slots.column = false;
  vi.mocked(toast.success).mockClear();
  vi.mocked(toast.warning).mockClear();
  stubWidth(true);
});

afterEach(() => {
  vi.unstubAllGlobals();
  widthSpy?.mockRestore();
  widthSpy = undefined;
});

describe("the page at 1024 px", () => {
  it("opens the change by its id: a breadcrumb back to the queue, the label and target, and the meta line", async () => {
    renderPage(change());
    const crumb = await screen.findByRole("link", { name: "Governance › Changes" });
    expect(crumb).toHaveAttribute("href", "/admin/governance?tab=changes");
    expect(getChangeMock).toHaveBeenCalledTimes(1);
    expect(getChangeMock).toHaveBeenCalledWith("c1");
    const title = screen.getByRole("heading", { level: 1 });
    expect(title).toHaveTextContent("Profile · Edit");
    expect(title).toHaveTextContent("team-a");
    expect(screen.getByText(CHANGES.META("rory@example.com", "2h ago", "in 20h"))).toBeInTheDocument();
  });

  it("draws the four tabs in order, Effect first, with the panel and the footer in the right column", async () => {
    renderPage(change());
    const tabs = await screen.findAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual([CHANGE_APPROVALS.TAB_EFFECT, CHANGE_REVIEW.IMPACT, CHANGE_REVIEW.RISK, CHANGE_REVIEW.RAW_DIFF]);
    expect(tabs[0]).toHaveAttribute("aria-selected", "true");
    expect(screen.getByTestId("change-review-page")).toHaveAttribute("data-layout", "tabs");
    const panel = screen.getByTestId("change-approvals");
    expect(follows(panel, screen.getByTestId("change-decision-footer"))).toBe(true);
    expect(follows(screen.getByRole("tabpanel"), panel)).toBe(true);
  });

  it("Effect holds the verdict, the counts row, the risk chip and the note, and not the other sections", async () => {
    renderPage(change({ note: { reason: "Raise it for the release", ticket: "SUP-1" } }));
    const effect = await screen.findByRole("tabpanel");
    expect(within(effect).getByTestId("change-verdict")).toBeInTheDocument();
    expect(within(effect).getByText("This lets runs under team-a reach 1 new host.")).toBeInTheDocument();
    expect(await within(effect).findByTestId("change-impact-summary")).toHaveTextContent("0 units · 3 live runs whose walls change");
    expect(within(effect).getByText("Risk Low → Medium")).toBeInTheDocument();
    expect(within(effect).getByTestId("change-note")).toHaveTextContent("Raise it for the release");
    expect(within(effect).queryByTestId("change-impact")).toBeNull();
    expect(within(effect).queryByTestId("change-risk")).toBeNull();
    expect(within(effect).queryByTestId("change-raw-diff")).toBeNull();
  });

  it("Impact, Risk and Raw diff each show their own section, and Raw diff is open", async () => {
    renderPage(change());
    await screen.findByRole("tabpanel");
    await userEvent.click(screen.getByRole("tab", { name: CHANGE_REVIEW.IMPACT }));
    expect(await screen.findByTestId("change-impact")).toBeInTheDocument();
    expect(screen.queryByTestId("change-verdict")).toBeNull();
    await userEvent.click(screen.getByRole("tab", { name: CHANGE_REVIEW.RISK }));
    expect(screen.getByTestId("change-risk")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: CHANGE_REVIEW.RAW_DIFF }));
    const raw = screen.getByTestId("change-raw-diff");
    expect(raw).toHaveAttribute("open");
    expect(within(raw).getByText("ceiling.allowed_domains")).toBeInTheDocument();
    expect(within(raw).getByText('["*.corp","git.x"]')).toBeInTheDocument();
    expect(within(raw).getByText(CHANGES.DIFF_FULL)).toBeInTheDocument();
  });

  it("reads the change once and its impact once, for a pending change", async () => {
    renderPage(change());
    await screen.findByTestId("change-impact-summary");
    expect(getChangeMock).toHaveBeenCalledTimes(1);
    expect(impactMock).toHaveBeenCalledTimes(1);
    expect(impactMock).toHaveBeenCalledWith("c1");
  });

  it("reads no impact for a change that is not pending", async () => {
    renderPage(change({ state: "applied", approvals: undefined }));
    await screen.findByRole("heading", { level: 1 });
    expect(impactMock).not.toHaveBeenCalled();
  });

  it("says what became of a change that is no longer waiting, with no buttons", async () => {
    renderPage(change({ state: "rejected", approvals: undefined }));
    expect(await screen.findByText(CHANGE_APPROVALS.DECIDED.rejected)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: CHANGES.APPROVE })).toBeNull();
    expect(screen.queryByTestId("change-approvals")).toBeNull();
  });

  it.each([
    ["applied", CHANGE_APPROVALS.DECIDED.applied],
    ["rejected", CHANGE_APPROVALS.DECIDED.rejected],
    ["expired", CHANGE_APPROVALS.DECIDED.expired],
  ] as const)("says a %s change is no longer waiting, in its own sentence", async (state, sentence) => {
    renderPage(change({ state, approvals: undefined }));
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    expect(screen.getAllByText(/^This change (was|expired)/)).toHaveLength(1);
  });

  it("shows the console's not-found page for an unknown id", async () => {
    renderPage(new HttpError(404, "no such change"), "bo@example.com", "/admin/governance/changes/nope");
    expect(await screen.findByText(PAGE_NOT_FOUND.TITLE)).toBeInTheDocument();
    expect(screen.queryByTestId("change-review-page")).toBeNull();
  });

  it("shows the server's sentence when the change cannot be read, and reads it again on Retry", async () => {
    renderPage(new HttpError(500, "the store is unavailable", "store_unavailable"));
    expect(await screen.findByText("the store is unavailable")).toBeInTheDocument();
    getChangeMock.mockResolvedValue(change());
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { level: 1 })).toBeInTheDocument();
  });
});

describe("the page at 390 px", () => {
  beforeEach(() => stubWidth(false));

  it("has no tab strip: verdict, impact, risk, the panel, then the raw diff closed", async () => {
    renderPage(change());
    const impact = await screen.findByTestId("change-impact");
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.getByTestId("change-review-page")).toHaveAttribute("data-layout", "stacked");
    const order = [screen.getByTestId("change-verdict"), impact, screen.getByTestId("change-risk"), screen.getByTestId("change-approvals"), screen.getByTestId("change-raw-diff")];
    for (let i = 1; i < order.length; i++) expect(follows(order[i - 1], order[i])).toBe(true);
    expect(order[4]).not.toHaveAttribute("open");
  });

  it("pins Reject and Approve to a bar at the bottom and keeps the reason box above it", async () => {
    renderPage(change());
    const bar = await screen.findByTestId("change-decision-bar");
    expect(bar).toHaveAttribute("data-pinned", "true");
    expect(bar).toContainElement(approveButton());
    expect(bar).toContainElement(rejectButton());
    const reason = screen.getByLabelText(CHANGE_APPROVALS.REASON_REQUIRED_LABEL);
    expect(bar).not.toContainElement(reason);
    expect(follows(screen.getByTestId("change-approvals"), reason)).toBe(true);
  });

  it("splits on the page's own width, so a window wide enough for tabs can still stack the page", async () => {
    // A tablet window with the sidebar open: the page is 500 px wide inside it.
    widthSpy?.mockReturnValue(500);
    renderPage(change());
    await screen.findByTestId("change-approvals");
    expect(screen.getByTestId("change-review-page")).toHaveAttribute("data-layout", "stacked");
  });

  it("puts the notes after Risk and before the reason", async () => {
    const notes = ["first note", "second note"];
    renderPage(change({ diff: { ...change().diff, notes } }));
    const heading = await screen.findByText(GOVERNANCE.OMISSION_TITLE);
    const reason = screen.getByLabelText(CHANGE_APPROVALS.REASON_REQUIRED_LABEL);
    expect(follows(screen.getByTestId("change-risk"), heading)).toBe(true);
    expect(follows(screen.getByText(notes[1]), reason)).toBe(true);
    expect(follows(screen.getByText(notes[0]), screen.getByText(notes[1]))).toBe(true);
  });
});

describe("the width edge", () => {
  it.each([
    [639, "stacked"],
    [640, "tabs"],
  ] as const)("a page %i px wide is laid out as %s", async (px, layout) => {
    stubWidth(true);
    widthSpy?.mockReturnValue(px);
    renderPage(change());
    await screen.findByTestId("change-approvals");
    expect(screen.getByTestId("change-review-page")).toHaveAttribute("data-layout", layout);
  });
});

describe("the tab strip", () => {
  it("moves between the tabs with the arrow keys, and every tab points at the one panel in the page", async () => {
    renderPage(change());
    const effect = await screen.findByRole("tab", { name: CHANGE_APPROVALS.TAB_EFFECT });
    effect.focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(await screen.findByRole("tab", { name: CHANGE_REVIEW.IMPACT, selected: true })).toHaveFocus();
    const panel = screen.getByRole("tabpanel");
    for (const t of screen.getAllByRole("tab")) expect(t.getAttribute("aria-controls")).toBe(panel.id);
    expect(panel.getAttribute("aria-labelledby")).toBe(screen.getByRole("tab", { name: CHANGE_REVIEW.IMPACT }).id);
  });
});

describe("deciding", () => {
  it("approving the last approval applies the change, toasts and returns to the queue, sending the reason", async () => {
    approveMock.mockResolvedValue(change({ state: "applied", approvals: undefined }));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    setField(screen.getByLabelText(CHANGE_APPROVALS.REASON_REQUIRED_LABEL), "hosts match the ticket");
    await userEvent.click(approveButton());
    expect(approveMock).toHaveBeenCalledWith("c1", "hosts match the ticket");
    expect(toast.success).toHaveBeenCalledWith(CHANGES.TOAST_APPROVED);
    expect(await screen.findByTestId("where")).toHaveTextContent("/admin/governance?tab=changes");
  });

  it("approving with no reason sends none", async () => {
    approveMock.mockResolvedValue(change({ state: "applied", approvals: undefined }));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await userEvent.click(approveButton());
    expect(approveMock).toHaveBeenCalledWith("c1", undefined);
  });

  it("an approval that leaves the change pending reads it again in place and stays", async () => {
    const after = change({ approvals: { ...SAMPLE_APPROVALS, have: 2, viewer: { state: "approved", approved_at: aheadByHours(0) } } });
    approveMock.mockResolvedValue({ ...after, state: "pending" });
    renderPage(change());
    await screen.findByTestId("change-approvals");
    getChangeMock.mockResolvedValue(after);
    await userEvent.click(approveButton());
    expect(await screen.findByRole("button", { name: CHANGE_APPROVALS.TAKE_BACK })).toBeInTheDocument();
    expect(getChangeMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByTestId("where")).toBeNull();
    expect(screen.getByTestId("change-approvals-progress")).toHaveTextContent("2 of 2 approved");
  });

  it("an applied approval whose token revocation did not finish keeps the server's remedy until dismissed", async () => {
    const detail = "The change is saved, but Wardyn could not revoke every API token that may still hold the old role.";
    approveMock.mockResolvedValue({ ...change({ state: "applied", approvals: undefined }), tokens_revocation_failed: true, detail });
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await userEvent.click(approveButton());
    expect(toast.warning).toHaveBeenCalledWith(detail, { duration: Infinity });
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("an applied suspension that could not end every run keeps the server's remedy until dismissed", async () => {
    const detail = "The suspension is applied, but Wardyn could not end every live run of the service identity.";
    approveMock.mockResolvedValue({ ...change({ state: "applied", approvals: undefined }), runs_end_failed: true, detail });
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await userEvent.click(approveButton());
    expect(toast.warning).toHaveBeenCalledWith(detail, { duration: Infinity });
  });

  it("rejecting with the typed reason toasts and returns to the queue", async () => {
    rejectMock.mockResolvedValue(change({ state: "rejected" }));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    setField(screen.getByLabelText(CHANGE_APPROVALS.REASON_REQUIRED_LABEL), "not now");
    await userEvent.click(rejectButton());
    expect(rejectMock).toHaveBeenCalledWith("c1", "not now");
    expect(toast.success).toHaveBeenCalledWith(CHANGES.TOAST_REJECTED);
    expect(await screen.findByTestId("where")).toHaveTextContent("/admin/governance?tab=changes");
  });

  it("rejecting with a required reason left empty writes the sentence and calls nothing", async () => {
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await userEvent.click(rejectButton());
    expect(screen.getByTestId("change-reason-missing")).toHaveTextContent(CHANGE_APPROVALS.REASON_NEEDED);
    expect(rejectMock).not.toHaveBeenCalled();
  });

  it("taking back an approval calls the take-back, toasts and reads the change again in place", async () => {
    const mine = change({ approvals: { ...SAMPLE_APPROVALS, viewer: { state: "approved", approved_at: aheadByHours(-0.2) } } });
    takeBackMock.mockResolvedValue(change());
    renderPage(mine);
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_APPROVALS.TAKE_BACK }));
    expect(takeBackMock).toHaveBeenCalledWith("c1");
    expect(toast.success).toHaveBeenCalledWith(CHANGE_APPROVALS.TOAST_TAKEN_BACK);
    await waitFor(() => expect(getChangeMock).toHaveBeenCalledTimes(2));
  });

  it("the proposer withdraws with no reason, and the answer returns them to the queue", async () => {
    rejectMock.mockResolvedValue(change({ state: "rejected" }));
    renderPage(change({ approvals: { ...SAMPLE_APPROVALS, viewer: { state: "proposer" } } }), "rory@example.com");
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_APPROVALS.WITHDRAW }));
    expect(rejectMock).toHaveBeenCalledWith("c1", "");
    expect(await screen.findByTestId("where")).toBeInTheDocument();
  });

  it.each([403, 409])("shows the server's own sentence on a %i, under the frozen heading, and stays", async (status) => {
    approveMock.mockRejectedValue(new HttpError(status, "the change you reviewed is no longer current"));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await userEvent.click(approveButton());
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(CHANGES.DECIDE_REFUSED_TITLE)).toBeInTheDocument();
    expect(within(alert).getByText("the change you reviewed is no longer current")).toBeInTheDocument();
    expect(toast.success).not.toHaveBeenCalled();
    expect(screen.queryByTestId("where")).toBeNull();
  });

  it("a 409 that finds the change decided already still shows the server's sentence", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "This change is no longer waiting for a decision.", "governance_change_not_pending"));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    getChangeMock.mockResolvedValue(change({ state: "rejected", approvals: undefined }));
    await userEvent.click(approveButton());
    expect(await screen.findByText(CHANGE_APPROVALS.DECIDED.rejected)).toBeInTheDocument();
    const alert = screen.getByRole("alert");
    expect(within(alert).getByText(CHANGES.DECIDE_REFUSED_TITLE)).toBeInTheDocument();
    expect(within(alert).getByText("This change is no longer waiting for a decision.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: CHANGES.APPROVE })).toBeNull();
  });

  it("a 409 reads the change again, since it moved", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "decided already"));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await userEvent.click(approveButton());
    await waitFor(() => expect(getChangeMock).toHaveBeenCalledTimes(2));
  });

  it("the proposer-inactive refusal is the server's sentence, as for any other", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "The proposer is no longer active. Propose it again.", "governance_change_proposer_inactive"));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await userEvent.click(approveButton());
    expect(await screen.findByText("The proposer is no longer active. Propose it again.")).toBeInTheDocument();
    expect(screen.queryByTestId("change-stale")).toBeNull();
  });
});

describe("focus and failures after a decision", () => {
  it("opens the note form from the Impact tab by going back to Effect, and gives the controls back after Cancel", async () => {
    renderPage(change({ approvals: { ...SAMPLE_APPROVALS, viewer: { state: "proposer" } } }), "rory@example.com");
    await userEvent.click(await screen.findByRole("tab", { name: CHANGE_REVIEW.IMPACT }));
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.ADD }));
    expect(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: CHANGE_APPROVALS.TAB_EFFECT })).toHaveAttribute("aria-selected", "true");
    expect(screen.queryByRole("button", { name: CHANGE_APPROVALS.WITHDRAW })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.CANCEL }));
    expect(screen.queryByLabelText(CHANGE_NOTE.WHY_LABEL)).toBeNull();
    expect(screen.getByRole("button", { name: CHANGE_APPROVALS.WITHDRAW })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: CHANGE_NOTE.ADD })).toBeInTheDocument();
  });

  it("moves focus to Take back after an approval that leaves the change pending", async () => {
    approveMock.mockResolvedValue(change());
    renderPage(change());
    await screen.findByTestId("change-approvals");
    getChangeMock.mockResolvedValue(change({ approvals: { ...SAMPLE_APPROVALS, viewer: { state: "approved", approved_at: aheadByHours(-0.1) } } }));
    await userEvent.click(approveButton());
    await waitFor(() => expect(screen.getByRole("button", { name: CHANGE_APPROVALS.TAKE_BACK })).toHaveFocus());
  });

  it("moves focus back to the buttons after a take-back", async () => {
    const mine = change({ approvals: { ...SAMPLE_APPROVALS, viewer: { state: "approved", approved_at: aheadByHours(-0.2) } } });
    takeBackMock.mockResolvedValue(change());
    renderPage(mine);
    const takeBack = await screen.findByRole("button", { name: CHANGE_APPROVALS.TAKE_BACK });
    getChangeMock.mockResolvedValue(change());
    await userEvent.click(takeBack);
    await waitFor(() => expect(rejectButton()).toHaveFocus());
  });

  it("moves focus to the banner when a refusal finds the change decided", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "No longer waiting.", "governance_change_not_pending"));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    getChangeMock.mockResolvedValue(change({ state: "rejected", approvals: undefined }));
    await userEvent.click(approveButton());
    await waitFor(() => expect(screen.getByTestId("change-decided")).toHaveFocus());
  });

  it("keeps the refusal on screen above the error when the read after a 409 fails", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "The change moved under you.", "governance_change_stale"));
    renderPage(change());
    await screen.findByTestId("change-approvals");
    getChangeMock.mockRejectedValue(new HttpError(500, "the store is unavailable", "store_unavailable"));
    await userEvent.click(approveButton());
    expect(await screen.findByText("the store is unavailable")).toBeInTheDocument();
    const alert = screen.getByText("The change moved under you.");
    expect(alert).toBeInTheDocument();
    expect(follows(alert, screen.getByText("the store is unavailable"))).toBe(true);
  });
});

describe("stale", () => {
  const STALE_IMPACT: ChangeImpact = { computed: "none", not_computed: "stale", as_of: aheadByHours(0) };

  it("before a decision, a profile change whose impact is stale says so and turns both buttons off", async () => {
    impactMock.mockResolvedValue(STALE_IMPACT);
    renderPage(change());
    const banner = await screen.findByTestId("change-stale");
    expect(banner).toHaveTextContent(
      "This change is stale: team-a changed after it was proposed. It can't apply, and its approvals are void. Ask rory@example.com to propose it again.",
    );
    expect(approveButton()).toBeDisabled();
    expect(rejectButton()).toBeDisabled();
  });

  it.each(["governance_membership", "governance_boundary"])("%s is replayed too, so its stale impact says stale", async (kind) => {
    impactMock.mockResolvedValue(STALE_IMPACT);
    renderPage(change({ target_kind: kind }));
    expect(await screen.findByTestId("change-stale")).toBeInTheDocument();
  });

  it("never infers it before a decision for a kind the impact route does not replay", async () => {
    impactMock.mockResolvedValue(STALE_IMPACT);
    renderPage(change({ target_kind: "role_mapping", target_key: "ops" }));
    await screen.findByTestId("change-approvals");
    await waitFor(() => expect(impactMock).toHaveBeenCalled());
    expect(screen.queryByTestId("change-stale")).toBeNull();
    expect(approveButton()).toBeEnabled();
  });

  it("is not read from an impact that is merely not computed", async () => {
    impactMock.mockResolvedValue({ computed: "none", not_computed: "kind", as_of: aheadByHours(0) });
    renderPage(change());
    await screen.findByTestId("change-approvals");
    await waitFor(() => expect(impactMock).toHaveBeenCalled());
    expect(screen.queryByTestId("change-stale")).toBeNull();
  });

  it("any kind reads stale once its own state says so, with both buttons off", async () => {
    renderPage(change({ target_kind: "role_mapping", target_key: "ops", state: "stale" }));
    expect(await screen.findByTestId("change-stale")).toBeInTheDocument();
    expect(approveButton()).toBeDisabled();
    expect(rejectButton()).toBeDisabled();
  });

  it("any kind reads stale once a decision answers 409 governance_change_stale", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "this change is stale", "governance_change_stale"));
    renderPage(change({ target_kind: "role_mapping", target_key: "ops" }));
    await screen.findByTestId("change-approvals");
    await userEvent.click(approveButton());
    expect(await screen.findByTestId("change-stale")).toBeInTheDocument();
    expect(approveButton()).toBeDisabled();
    expect(rejectButton()).toBeDisabled();
  });
});

describe("a change one person decides", () => {
  it("shows One person decides in the panel, and today's Approve and Reject", async () => {
    renderPage(change({ approvals: undefined, approvers: [{ kind: "security" }] }));
    expect(await screen.findByText(CHANGE_APPROVALS.ONE_PERSON)).toBeInTheDocument();
    expect(screen.queryByTestId("change-approvals-progress")).toBeNull();
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(approveButton()).toBeEnabled();
    expect(rejectButton()).toBeEnabled();
    expect(screen.queryByTestId("change-viewer-status")).toBeNull();
  });

  it("an own change that only security can decide keeps its note, Reject on and Approve off", async () => {
    renderPage(change({ approvals: undefined, approvers: [{ kind: "security" }] }), "rory@example.com");
    expect(await screen.findByText(CHANGE_NOTE.OWN_SECURITY_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(CHANGES.OWN_NOTE)).toBeNull();
    expect(approveButton()).toBeDisabled();
    expect(rejectButton()).toBeEnabled();
  });

  it("an own change keeps OWN_NOTE when anyone but security can decide", async () => {
    renderPage(change({ approvals: undefined, approvers: [{ kind: "security" }, { kind: "unit_admin", unit_name: "Payments" }] }), "rory@example.com");
    expect(await screen.findByText(CHANGES.OWN_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(CHANGE_NOTE.OWN_SECURITY_TITLE)).toBeNull();
  });
});

describe("the proposer's note", () => {
  it("shows the note as plain text after the amber notes and before the reason", async () => {
    const c = change({
      approvals: undefined,
      note: { reason: "<b>not html</b> https://example.com", ticket: "https://example.com/t/1" },
      diff: { ...change().diff, notes: ["omits the egress list"] },
    });
    renderPage(c);
    const block = await screen.findByTestId("change-note");
    expect(within(block).getByRole("heading", { name: CHANGE_NOTE.WHY_TITLE })).toBeInTheDocument();
    expect(block).toHaveTextContent("<b>not html</b> https://example.com");
    expect(block.querySelector("a, b")).toBeNull();
    expect(within(block).getByText("https://example.com/t/1").className).toContain("font-mono");
    expect(follows(screen.getByText(GOVERNANCE.OMISSION_TITLE), block)).toBe(true);
    expect(follows(block, screen.getByLabelText(CHANGES.REASON_LABEL))).toBe(true);
  });

  it("is offered Add a note only to the proposer, and Edit note when there is one", async () => {
    const { unmount } = renderPage(change({ approvals: undefined }));
    await screen.findByLabelText(CHANGES.REASON_LABEL);
    expect(screen.queryByRole("button", { name: CHANGE_NOTE.ADD })).toBeNull();
    unmount();
    renderPage(change({ approvals: undefined, note: { reason: "why" } }), "rory@example.com");
    expect(await screen.findByRole("button", { name: CHANGE_NOTE.EDIT })).toBeInTheDocument();
  });

  it("opens the form in place and hides the decider's reason and buttons while it is open", async () => {
    renderPage(change({ approvals: undefined }), "rory@example.com");
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_NOTE.ADD }));
    expect(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL)).toBeInTheDocument();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
    expect(screen.queryByRole("button", { name: CHANGES.REJECT })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.CANCEL }));
    expect(screen.getByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
  });

  it("a save toasts and shows the returned note on the page, keeping the approvals", async () => {
    noteMock.mockResolvedValue(change({ note: { reason: "Raise the cap", ticket: "CHG-1" } }));
    renderPage(change(), "rory@example.com");
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_NOTE.ADD }));
    setField(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL), "Raise the cap");
    setField(screen.getByLabelText(CHANGE_NOTE.TICKET_LABEL), "CHG-1");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(noteMock).toHaveBeenCalledWith("c1", { reason: "Raise the cap", ticket: "CHG-1" });
    expect(toast.success).toHaveBeenCalledWith(CHANGE_NOTE.SAVED);
    expect(await screen.findByTestId("change-note")).toHaveTextContent("Raise the cap");
    expect(screen.getByRole("button", { name: CHANGE_NOTE.EDIT })).toBeInTheDocument();
    expect(screen.getByTestId("change-approvals-progress")).toBeInTheDocument();
  });

  it("a 404 on save returns to the queue, and a 409 reads the change again and keeps the refusal", async () => {
    noteMock.mockRejectedValueOnce(new HttpError(409, "no longer pending")).mockRejectedValueOnce(new HttpError(404, "gone"));
    renderPage(change({ approvals: undefined }), "rory@example.com");
    await userEvent.click(await screen.findByRole("button", { name: CHANGE_NOTE.ADD }));
    setField(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL), "x");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(await screen.findByText("no longer pending")).toBeInTheDocument();
    await waitFor(() => expect(getChangeMock).toHaveBeenCalledTimes(2));
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(await screen.findByTestId("where")).toHaveTextContent("/admin/governance?tab=changes");
  });
});

describe("what the page renders from the server's diff", () => {
  it("renders an assignment's embedded profile summary from the diff, and looks nothing up", async () => {
    renderPage(
      change({
        approvals: undefined,
        target_kind: "governance_membership",
        op: "upsert",
        diff: {
          after: {
            subject_type: "group",
            subject: "g",
            unit_id: "p9",
            priority: 0,
            unit: { id: "p9", name: "Embedded profile", ceiling: { allowed_domains: ["a.example"] }, limits: {} },
          },
          changed: ["unit_id"],
        },
      }),
    );
    const section = await screen.findByTestId("change-assigned-profile");
    expect(within(section).getByText(CHANGES.ASSIGNED_PROFILE)).toBeInTheDocument();
    expect(within(section).getByText("Embedded profile")).toBeInTheDocument();
    expect(section.textContent).toContain("a.example");
  });

  it("on an assignment, puts the notes after the profile it assigns and before the reason", async () => {
    const notes = ["first note"];
    renderPage(
      change({
        approvals: undefined,
        target_kind: "governance_membership",
        op: "upsert",
        diff: {
          after: { subject_type: "group", subject: "g", unit_id: "p9", priority: 0, unit: { id: "p9", name: "Embedded profile", ceiling: {}, limits: {} } },
          changed: ["unit_id"],
          notes,
        },
      }),
    );
    const assigned = await screen.findByText(CHANGES.ASSIGNED_PROFILE);
    expect(follows(assigned, screen.getByText(GOVERNANCE.OMISSION_TITLE))).toBe(true);
    expect(follows(screen.getByText(notes[0]), screen.getByLabelText(CHANGES.REASON_LABEL))).toBe(true);
  });

  it("shows an absent side as Not set", async () => {
    renderPage(change({ approvals: undefined, op: "create", diff: { after: { name: "team-a" }, changed: ["name"] } }));
    await userEvent.click(await screen.findByRole("tab", { name: CHANGE_REVIEW.RAW_DIFF }));
    expect(within(screen.getByTestId("change-raw-diff")).getAllByText(CHANGES.DIFF_UNSET).length).toBeGreaterThan(0);
  });

  it("names a runner trust change by the runner's name and shows its fields on the raw diff", async () => {
    const id = "3f9c1c2e-7a41-4d0b-9e6a-5b2f0d8c1a77";
    renderPage(
      change({
        id,
        approvals: undefined,
        target_kind: "runner_trust",
        target_key: id,
        diff: { before: { name: "dana-laptop", trust_class: "personal" }, after: { name: "dana-laptop", trust_class: "managed_device" }, changed: ["trust_class"] },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1 })).toHaveTextContent("dana-laptop");
    await userEvent.click(screen.getByRole("tab", { name: CHANGE_REVIEW.RAW_DIFF }));
    const raw = screen.getByTestId("change-raw-diff");
    expect(within(raw).getByText("trust_class")).toBeInTheDocument();
    expect(within(raw).getByText("personal")).toBeInTheDocument();
    expect(within(raw).getByText("managed_device")).toBeInTheDocument();
  });

  it("shows no notes heading for a change that carries none", async () => {
    renderPage(change({ approvals: undefined }));
    await screen.findByTestId("change-verdict");
    expect(screen.queryByText(GOVERNANCE.OMISSION_TITLE)).toBeNull();
  });
});

describe("a request kind", () => {
  const elevation = (over: Partial<GovernanceChange> = {}) =>
    change({
      id: "e1",
      target_kind: "governance_elevation",
      op: "create",
      target_key: "e1",
      approvals: undefined,
      proposed_by: "alice",
      diff: { after: { unit_id: "u1", unit_name: "Payments", subject: "alice", duration: "4h" }, changed: ["duration"] },
      note: { reason: "Production incident", ticket: "SUP-1" },
      ...over,
    });

  it("reads Person, Unit and Length, the target in the title, and keeps the full diff", async () => {
    renderPage(elevation());
    const title = await screen.findByRole("heading", { level: 1 });
    expect(title).toHaveTextContent(/Elevation · New\s*alice → Payments/);
    const rows = screen.getAllByRole("row").map((r) => r.textContent);
    expect(rows).toEqual([
      `${CHANGE_NOTE.ELEV_PERSON}alice`,
      `${CHANGE_NOTE.ELEV_UNIT}Payments`,
      `${CHANGE_NOTE.ELEV_LENGTH}${CHANGE_NOTE.ELEV_LENGTH_VALUE("4 hours")}`,
    ]);
    expect(screen.getByText(CHANGES.DIFF_FULL)).toBeInTheDocument();
    expect(screen.getByTestId("change-note")).toHaveTextContent("Production incident");
  });

  it("has no tabs, no impact and no risk, and says one person decides in place of the panel", async () => {
    renderPage(elevation());
    await screen.findByRole("heading", { level: 1 });
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.getByText(CHANGE_APPROVALS.ONE_PERSON)).toBeInTheDocument();
    expect(screen.queryByTestId("change-verdict")).toBeNull();
    expect(screen.queryByTestId("change-impact")).toBeNull();
    expect(screen.queryByTestId("change-risk")).toBeNull();
    expect(impactMock).not.toHaveBeenCalled();
  });

  it("someone else's keeps Reject and Approve and the decider's reason", async () => {
    renderPage(elevation());
    expect(await screen.findByLabelText(CHANGES.REASON_LABEL)).toBeInTheDocument();
    expect(rejectButton()).toBeEnabled();
    expect(approveButton()).toBeEnabled();
    expect(screen.queryByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST })).toBeNull();
  });

  it("the person's own reads Withdraw request, with no reason field and Approve off, and posts no reason", async () => {
    rejectMock.mockResolvedValue({});
    renderPage(elevation(), "alice");
    expect(await screen.findByText(CHANGE_NOTE.OWN_REQUEST)).toBeInTheDocument();
    expect(screen.queryByLabelText(CHANGES.REASON_LABEL)).toBeNull();
    expect(approveButton()).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.WITHDRAW_REQUEST }));
    expect(rejectMock).toHaveBeenCalledWith("e1", "");
  });

  it("an image request reads its person, image, digest and unit", async () => {
    const ref = "registry.example/app@sha256:" + "a".repeat(64);
    renderPage(
      elevation({
        id: "i1",
        target_kind: "governance_image_request",
        diff: { after: { image: ref, unit_id: "u1", unit_name: "Payments", subject: "alice" }, changed: ["image"] },
        note: undefined,
      }),
    );
    expect(await screen.findByRole("heading", { level: 1 })).toHaveTextContent("Image · New");
    expect(screen.getAllByRole("row").length).toBe(5);
    expect(screen.queryByTestId("change-verdict")).toBeNull();
  });

  it("a request whose people it cannot read keeps its raw diff, open", async () => {
    renderPage(elevation({ diff: { after: { subject: "alice" }, changed: [] } }));
    expect(await screen.findByTestId("change-raw-diff")).toHaveAttribute("open");
  });
});

describe("the mount points for the review assistant", () => {
  it("renders the header chip and the sheet button once each with the change, at every width", async () => {
    renderPage(change());
    await screen.findByRole("heading", { level: 1 });
    expect(screen.getAllByTestId("slot-chip")).toHaveLength(1);
    expect(screen.getAllByTestId("slot-sheet")).toHaveLength(1);
    expect((slots.props.chip as GovernanceChange).id).toBe("c1");
    expect((slots.props.sheet as GovernanceChange).id).toBe("c1");
  });

  it("renders the replay slot once in the Impact tab, and once in the stacked order", async () => {
    const { unmount } = renderPage(change());
    await screen.findByRole("tabpanel");
    expect(screen.queryByTestId("slot-replay")).toBeNull();
    await userEvent.click(screen.getByRole("tab", { name: CHANGE_REVIEW.IMPACT }));
    const replay = screen.getByTestId("slot-replay");
    expect(screen.getAllByTestId("slot-replay")).toHaveLength(1);
    expect((slots.props.replay as GovernanceChange).id).toBe("c1");
    await screen.findByTestId("change-impact");
    expect(follows(screen.getByTestId("change-impact"), replay)).toBe(true);
    unmount();
    stubWidth(false);
    renderPage(change());
    const stackedReplay = await screen.findByTestId("slot-replay");
    expect(follows(await screen.findByTestId("change-impact"), stackedReplay)).toBe(true);
  });

  it("keeps the grid at two tracks, and no column, while the assistant column is off", async () => {
    renderPage(change());
    await screen.findByRole("heading", { level: 1 });
    const grid = screen.getByTestId("change-review-grid");
    expect(grid.className).toContain("minmax(0,1fr)_320px]");
    expect(grid.className).not.toContain("360px");
    expect(screen.queryByTestId("slot-column")).toBeNull();
  });

  it("gains the third track and renders the column after the panel when it is turned on", async () => {
    slots.column = true;
    renderPage(change());
    await screen.findByRole("heading", { level: 1 });
    expect(screen.getByTestId("change-review-grid").className).toContain("minmax(0,1fr)_320px_360px]");
    const column = screen.getByTestId("slot-column");
    expect(screen.getAllByTestId("slot-column")).toHaveLength(1);
    expect((slots.props.column as GovernanceChange).id).toBe("c1");
    expect(follows(screen.getByTestId("change-approvals"), column)).toBe(true);
  });
});
