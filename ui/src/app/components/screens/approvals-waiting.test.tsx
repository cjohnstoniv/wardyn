/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Approvals screen's "Waiting on you" tab, the deciders lines on its cards, the basis line on decided rows,
// the scope cap in the Approve dialog and the three refusal toasts. The api module is mocked wholesale, as
// approvals.test.tsx does; its own reads are pinned in lib/api/approvals-decidable.test.ts.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities } from "../../lib/types";
import { DECIDERS_INBOX } from "../wardyn/copy/deciders-inbox";
import { SECURITY_ONLY_REASON } from "../wardyn/copy";

const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { error: (...a: unknown[]) => toastError(...a), success: vi.fn() } }));

let mockKnows = true;
let mockPending: ApprovalRequest[] = [];
let mockWaiting: ApprovalRequest[] = [];
let mockTruncated = false;
let mockDecided: Partial<Record<string, ApprovalRequest[]>> = {};
const knowsMock = vi.fn(async () => mockKnows);
const waitingMock = vi.fn(async () => ({ rows: mockWaiting, truncated: mockTruncated }));
const approveMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: async (state: string) => (state === "PENDING" ? mockPending : (mockDecided[state] ?? [])),
    knowsDecidableBy: () => knowsMock(),
    listDecidableByMe: () => waitingMock(),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: vi.fn(),
  },
}));
let mockCaps: MeCapabilities = { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 };
vi.mock("../../lib/api/permissions", () => ({ permissions: { getMyCapabilities: () => Promise.resolve(mockCaps) } }));
const getRun = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => {
      getRun(...a);
      return Promise.resolve({ id: "run_1", agent: "claude-code", repo: "acme/widgets", task: "Fix tests", confinement_class: "CC2", state: "RUNNING" });
    },
  },
}));
let mockGovernance: () => Promise<unknown> = () => Promise.resolve({ unit_names: {}, profiles: [] });
const governanceMock = vi.fn(() => mockGovernance());
vi.mock("../../lib/api/governance", () => ({ governance: { getGovernance: () => governanceMock() } }));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider, RoleProvider } from "../wardyn/operator-context";
import { HttpError } from "../../lib/api/core";

const NOW = new Date().toISOString();
const egress = (id: string, extra: Partial<ApprovalRequest> = {}): ApprovalRequest => ({
  id,
  run_id: "4f2a91c0-1111-2222-3333-444455556666",
  kind: "egress_domain",
  requested_scope: { host: `${id}.example.com` },
  state: "PENDING",
  requested_at: NOW,
  ...extra,
});
const others = (id: string, extra: Partial<ApprovalRequest> = {}) =>
  egress(id, { requested_by: "dana.reyes@acme.example", deciders: ["owner", "unit_admins", "security"], ...extra });

function LocationProbe() {
  return <span data-testid="search">{useLocation().search}</span>;
}
function renderScreen({ security = false, url = "/approvals" }: { security?: boolean; url?: string } = {}) {
  return render(
    <OperatorProvider operator={false} securityOperator={security}>
      <RoleProvider role="user">
        <MemoryRouter initialEntries={[url]}>
          <ApprovalsScreen />
          <LocationProbe />
        </MemoryRouter>
      </RoleProvider>
    </OperatorProvider>,
  );
}
const waitingTab = () => screen.queryByRole("tab", { name: /^Waiting on you/ /* APPROVALS_TAB.WAITING */ });

beforeEach(() => {
  mockKnows = true;
  mockPending = [];
  mockWaiting = [];
  mockTruncated = false;
  mockDecided = {};
  mockCaps = { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 };
  mockGovernance = () => Promise.resolve({ unit_names: {}, profiles: [] });
  toastError.mockClear();
  knowsMock.mockClear();
  waitingMock.mockClear();
  approveMock.mockReset();
  getRun.mockClear();
  governanceMock.mockClear();
});
afterEach(() => vi.useRealTimers());

describe("Waiting on you — who gets the tab", () => {
  it("a security operator never gets it, even with rows that carry requested_by", async () => {
    mockPending = [egress("p1")];
    mockWaiting = [others("w1")];
    renderScreen({ security: true });
    await screen.findByRole("tab", { name: /^Pending/ });
    await act(async () => {});
    expect(waitingTab()).not.toBeInTheDocument();
    expect(knowsMock).not.toHaveBeenCalled();
    expect(waitingMock).not.toHaveBeenCalled();
  });

  it("an older server (the probe says it does not know decidable_by) gets no tab", async () => {
    mockKnows = false;
    mockWaiting = [others("w1")];
    renderScreen();
    await waitFor(() => expect(knowsMock).toHaveBeenCalledTimes(1));
    await act(async () => {});
    expect(waitingTab()).not.toBeInTheDocument();
    expect(waitingMock).not.toHaveBeenCalled();
  });

  it("a probe that fails draws no tab and nothing else", async () => {
    knowsMock.mockRejectedValueOnce(new Error("503"));
    mockWaiting = [others("w1")];
    renderScreen();
    await screen.findByRole("tab", { name: /^Pending/ });
    await act(async () => {});
    expect(waitingTab()).not.toBeInTheDocument();
  });

  it("no tab until a row carries requested_by; drawn after, and still drawn when a later read has none", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    mockWaiting = [egress("own1", { deciders: ["owner", "group:eng-leads"] })];
    renderScreen();
    await waitFor(() => expect(waitingMock).toHaveBeenCalledTimes(1));
    await act(async () => {});
    expect(waitingTab()).not.toBeInTheDocument();

    mockWaiting = [others("w1")];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());

    mockWaiting = [];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(waitingMock.mock.calls.length).toBeGreaterThanOrEqual(3);
    expect(waitingTab()).toBeInTheDocument();
  });

  it("the badge equals the row count, and there is none at zero", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    mockWaiting = [others("w1"), others("w2"), others("w3")];
    renderScreen();
    const tab = await waitFor(() => {
      const t = waitingTab();
      expect(t).toBeInTheDocument();
      return t!;
    });
    expect(tab).toHaveTextContent(/Waiting on you\s*3$/);
    mockWaiting = [];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    await waitFor(() => expect(waitingTab()).toHaveTextContent(/^Waiting on you$/));
  });

  it("the help line shows only while the tab is drawn", async () => {
    renderScreen();
    await screen.findByRole("tab", { name: /^Pending/ });
    await act(async () => {});
    expect(screen.queryByText(DECIDERS_INBOX.HELP)).not.toBeInTheDocument();
  });

  it("the help line shows with the tab", async () => {
    mockWaiting = [others("w1")];
    renderScreen();
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());
    expect(screen.getByText(DECIDERS_INBOX.HELP)).toBeInTheDocument();
  });

  it("?tab=waiting with the tab not drawn shows Pending", async () => {
    mockKnows = false;
    mockPending = [egress("p1")];
    renderScreen({ url: "/approvals?tab=waiting" });
    expect(await screen.findByText("Reach p1.example.com")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: /^Pending/ })).toHaveAttribute("data-state", "active");
  });

  it("?tab=waiting round-trips: it opens the tab once drawn and the tab writes it back", async () => {
    mockWaiting = [others("w1")];
    renderScreen({ url: "/approvals?tab=waiting" });
    await waitFor(() => expect(waitingTab()).toHaveAttribute("data-state", "active"));
    await userEvent.click(screen.getByRole("tab", { name: /^Pending/ }));
    expect(screen.getByTestId("search")).toHaveTextContent("");
    await userEvent.click(waitingTab()!);
    expect(screen.getByTestId("search")).toHaveTextContent("?tab=waiting");
  });
});

describe("Waiting on you — the tab's body", () => {
  it("lists the rows with the Requested by row, no Open run link and no run read", async () => {
    mockWaiting = [others("w1"), others("w2", { second_human: true })];
    renderScreen();
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());
    await userEvent.click(waitingTab()!);
    expect(await screen.findByText("Reach w1.example.com")).toBeInTheDocument();
    expect(screen.getAllByText("dana.reyes@acme.example")).toHaveLength(2);
    expect(screen.getAllByText("4f2a91c0")).toHaveLength(2);
    expect(screen.getAllByText(DECIDERS_INBOX.CANT_OPEN_RUN)).toHaveLength(2);
    expect(screen.queryByRole("link", { name: /open run/i })).not.toBeInTheDocument();
    expect(screen.getAllByText(DECIDERS_INBOX.DECIDED_BY_LABEL)[0].closest("p")).toHaveTextContent(
      "Decided by: the run's owner, admins of this unit or a security admin",
    );
    expect(screen.getAllByText(DECIDERS_INBOX.SECOND_PERSON)).toHaveLength(1);
    expect(getRun).not.toHaveBeenCalled();
  });

  it("an own row on the tab still reads the run (it can be opened) and says you", async () => {
    mockWaiting = [others("w1"), egress("own1", { deciders: ["owner", "group:eng-leads"] })];
    renderScreen();
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());
    await userEvent.click(waitingTab()!);
    await screen.findByText("Reach own1.example.com");
    await waitFor(() => expect(getRun).toHaveBeenCalledTimes(1));
    expect(screen.getByText(/^Decided by:/, { selector: "span" }).closest("p")).toBeTruthy();
    expect(screen.getByText("you, the eng-leads group", { exact: false })).toBeInTheDocument();
  });

  it("Approve is enabled for a non-security decider on someone else's egress row, even under an ungranted host", async () => {
    mockCaps = { ...mockCaps, enforcement: { egress_host: true } };
    mockWaiting = [others("w1")];
    renderScreen();
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());
    await userEvent.click(waitingTab()!);
    await screen.findByText("Reach w1.example.com");
    expect(screen.getByRole("button", { name: /^approve$/i })).toBeEnabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeEnabled();
  });

  it("empty: the state with See decided, which opens the Decided tab", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    mockWaiting = [others("w1")];
    renderScreen();
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());
    await userEvent.click(waitingTab()!, { advanceTimers: vi.advanceTimersByTimeAsync });
    mockWaiting = [];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(await screen.findByText(DECIDERS_INBOX.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DECIDERS_INBOX.EMPTY_BODY)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: DECIDERS_INBOX.EMPTY_ACTION }), { advanceTimers: vi.advanceTimersByTimeAsync });
    expect(screen.getByRole("tab", { name: /^Decided/ })).toHaveAttribute("data-state", "active");
  });

  it("truncated: the note sits above the cards and the cards stay", async () => {
    mockTruncated = true;
    mockWaiting = [others("w1")];
    renderScreen();
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());
    await userEvent.click(waitingTab()!);
    const note = await screen.findByText(DECIDERS_INBOX.TRUNCATED);
    const card = screen.getByText("Reach w1.example.com");
    expect(note.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("a failed poll keeps the cards on screen", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    mockWaiting = [others("w1")];
    renderScreen();
    await waitFor(() => expect(waitingTab()).toBeInTheDocument());
    await userEvent.click(waitingTab()!, { advanceTimers: vi.advanceTimersByTimeAsync });
    await screen.findByText("Reach w1.example.com");
    waitingMock.mockRejectedValueOnce(new Error("503"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(screen.getByText("Reach w1.example.com")).toBeInTheDocument();
  });
});

describe("Pending cards — who decides", () => {
  it("an own card whose rule leaves the owner out draws the pair disabled and says someone else decides", async () => {
    mockKnows = false;
    mockPending = [egress("p1", { deciders: ["unit_admins", "security"] })];
    renderScreen();
    await screen.findByText("Reach p1.example.com");
    expect(screen.getByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    expect(screen.getByText(DECIDERS_INBOX.SOMEONE_ELSE)).toBeInTheDocument();
    expect(screen.queryByText(SECURITY_ONLY_REASON)).not.toBeInTheDocument();
    expect(screen.getByText(DECIDERS_INBOX.DECIDED_BY_LABEL).closest("p")).toHaveTextContent(
      "Decided by: admins of this unit or a security admin",
    );
  });

  it("an own card whose rule names the owner keeps working buttons", async () => {
    mockKnows = false;
    mockPending = [egress("p1", { deciders: ["owner", "group:eng-leads"] })];
    renderScreen();
    await screen.findByText("Reach p1.example.com");
    expect(screen.getByRole("button", { name: /^approve$/i })).toBeEnabled();
    expect(screen.queryByText(DECIDERS_INBOX.SOMEONE_ELSE)).not.toBeInTheDocument();
  });

  it("a card with no deciders draws none of the new lines", async () => {
    mockKnows = false;
    mockPending = [egress("p1")];
    renderScreen();
    await screen.findByText("Reach p1.example.com");
    expect(screen.queryByText(DECIDERS_INBOX.DECIDED_BY_LABEL)).not.toBeInTheDocument();
    expect(screen.queryByText(DECIDERS_INBOX.CANT_OPEN_RUN)).not.toBeInTheDocument();
  });

  it("the Approve dialog offers nothing wider than the rule's max_scope", async () => {
    mockKnows = false;
    mockPending = [egress("p1", { deciders: ["owner"], max_scope: "until" })];
    renderScreen();
    await screen.findByText("Reach p1.example.com");
    await userEvent.click(screen.getByRole("button", { name: /^approve$/i }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("radio", { name: /^This run/ })).toBeDisabled();
    expect(within(dialog).getByRole("radio", { name: /^Always/ })).toBeDisabled();
    expect(within(dialog).getByRole("radio", { name: /^Until/ })).toBeEnabled();
  });
});

describe("Decided rows — the basis line", () => {
  const decided = (id: string, basis?: string): ApprovalRequest => ({
    ...egress(id),
    state: "APPROVED",
    decided_by: "dana.reyes",
    decided_at: NOW,
    ...(basis !== undefined ? { decision_basis: basis } : {}),
  });
  const openDecided = async () => {
    renderScreen();
    await userEvent.click(await screen.findByRole("tab", { name: /^Decided/ }));
  };

  it("says each of the five fixed bases, and none for an absent or unknown basis", async () => {
    mockKnows = false;
    mockDecided.APPROVED = [
      decided("a", "owner"),
      decided("b", "group:eng-leads"),
      decided("c", "security"),
      decided("d", "policy"),
      decided("e", "admin_token"),
      decided("f"),
      decided("g", "mystery"),
    ];
    await openDecided();
    await screen.findByText("Reach a.example.com");
    expect(screen.getByText("Decided as the run's owner")).toBeInTheDocument();
    expect(screen.getByText("Decided as a member of the eng-leads group")).toBeInTheDocument();
    expect(screen.getByText("Decided as a security admin")).toBeInTheDocument();
    expect(screen.getByText("Decided by an approval rule, not a person")).toBeInTheDocument();
    expect(screen.getByText("Decided with the admin token")).toBeInTheDocument();
    expect(screen.getAllByText(/^Decided (as|by|with)/)).toHaveLength(5);
    // No unit-admin basis on screen: nothing asked for names.
    expect(governanceMock).not.toHaveBeenCalled();
    // The by/when meta is unchanged.
    expect(screen.getAllByText(/^by dana\.reyes · /)).toHaveLength(7);
  });

  it("names the unit for unit_admin:<id> from the governance names and the listed profiles", async () => {
    mockKnows = false;
    mockGovernance = () => Promise.resolve({ unit_names: { u1: "Payments" }, profiles: [{ id: "u2", name: "Platform" }] });
    mockDecided.APPROVED = [decided("a", "unit_admin:u1"), decided("b", "unit_admin:u2")];
    await openDecided();
    expect(await screen.findByText("Decided as an admin of Payments")).toBeInTheDocument();
    expect(screen.getByText("Decided as an admin of Platform")).toBeInTheDocument();
    expect(governanceMock).toHaveBeenCalledTimes(1);
  });

  it("falls back to a unit admin when the name is unknown or the governance read is refused, and does not retry", async () => {
    mockKnows = false;
    let refuse!: (e: unknown) => void;
    mockGovernance = () => new Promise((_, rej) => (refuse = rej));
    mockDecided.APPROVED = [decided("a", "unit_admin:u1")];
    await openDecided();
    await screen.findByText("Reach a.example.com");
    expect(screen.getByText("Decided as a unit admin")).toBeInTheDocument();
    await act(async () => refuse(new HttpError(403, "forbidden")));
    expect(screen.getByText("Decided as a unit admin")).toBeInTheDocument();
    expect(governanceMock).toHaveBeenCalledTimes(1);
  });
});

describe("Refusal toasts", () => {
  const openApprove = async () => {
    mockKnows = false;
    mockPending = [egress("p1", { deciders: ["owner"] })];
    renderScreen();
    await screen.findByText("Reach p1.example.com");
    await userEvent.click(screen.getByRole("button", { name: /^approve$/i }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: /^approve$/i }));
  };

  it("not_a_decider reads as its own toast with the server's sentence", async () => {
    approveMock.mockRejectedValue(new HttpError(403, "only the run's owner or an admin of this unit decides this", "not_a_decider"));
    await openApprove();
    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith(DECIDERS_INBOX.TOAST_NOT_A_DECIDER, {
        description: "only the run's owner or an admin of this unit decides this",
      }),
    );
    expect(DECIDERS_INBOX.TOAST_NOT_A_DECIDER).toBe("Someone else decides this request");
  });

  it("decision_scope_above_rule reads as its own toast with the server's sentence", async () => {
    approveMock.mockRejectedValue(new HttpError(403, "this unit allows nothing wider than until", "decision_scope_above_rule"));
    await openApprove();
    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith(DECIDERS_INBOX.TOAST_SCOPE_ABOVE_RULE, {
        description: "this unit allows nothing wider than until",
      }),
    );
    expect(DECIDERS_INBOX.TOAST_SCOPE_ABOVE_RULE).toBe("That choice is wider than this unit allows");
  });

  it("deciders_unreadable reads as its own toast with a fixed body", async () => {
    approveMock.mockRejectedValue(new HttpError(503, "could not read deciders", "deciders_unreadable"));
    await openApprove();
    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith(DECIDERS_INBOX.TOAST_UNREADABLE, {
        description: DECIDERS_INBOX.TOAST_UNREADABLE_BODY,
      }),
    );
    expect(DECIDERS_INBOX.TOAST_UNREADABLE).toBe("Couldn't check who decides this");
    expect(DECIDERS_INBOX.TOAST_UNREADABLE_BODY).toBe("Nothing was decided. Try again in a moment.");
  });

  it("any other error keeps today's toast", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "already decided", "conflict"));
    await openApprove();
    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError.mock.calls[0][0]).not.toBe(DECIDERS_INBOX.TOAST_NOT_A_DECIDER);
    expect(toastError.mock.calls[0][1]).toEqual({ description: "already decided" });
  });
});
