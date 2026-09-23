/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import type { AgentRun, AuditEvent, RunState } from "../../../lib/types";
import { RunCard } from "./run-card";
import { approvalSignals, type RunSignals } from "./board-groups";
import { RUN } from "../../wardyn/copy";
import { CLONE_LOAD_FAILED } from "../new-run/wizard-types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { waitingAdoConsent, waitingReauth } from "../../../lib/reauth-waiting-copy";

// review C-01/C-06/C-07 — cloneRun's own behaviour, not just the menu item's
// gating. listAudit is stubbed; createRequestFromAudit stays REAL so the
// happy-path test proves an audit-derived field (tool_approvals) actually
// reaches the prefill, not just that SOME object was passed.
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/audit")>("../../../lib/api/audit");
  return { ...actual, audit: { ...actual.audit, listAudit: (...a: unknown[]) => listAuditMock(...a) } };
});

const navigateMock = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => navigateMock };
});

vi.mock("sonner", () => ({ toast: { error: vi.fn(), warning: vi.fn() } }));

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
  id: "run_3b7f10c4aa99",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/payments-api",
  task: "Rotate the staging credentials",
  confinement_class: "CC3",
  state: "RUNNING" as RunState,
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  ...over,
});

function renderCard(r: AgentRun, signals: RunSignals = new Map(), principal = "") {
  return render(
    <OperatorProvider operator principal={principal}>
      <MemoryRouter>
        <RunCard run={r} signals={signals} onOpen={vi.fn()} onKill={vi.fn()} />
      </MemoryRouter>
    </OperatorProvider>,
  );
}

const reauthSignals = (runId: string) =>
  approvalSignals([
    {
      id: "a-reauth",
      run_id: runId,
      kind: "credential_reauth",
      requested_scope: { mechanism: "bedrock_sso", credential_source: "per_user", owner: "me" },
      state: "PENDING",
      requested_at: new Date().toISOString(),
    },
  ]);

// S10 round 2 (F13) — the Azure DevOps twin of reauthSignals: same wire kind
// (credential_reauth), a DIFFERENT provider, so the board chip must read
// "Azure DevOps", never "AWS".
const adoConsentSignals = (runId: string) =>
  approvalSignals([
    {
      id: "a-ado-consent",
      run_id: runId,
      kind: "credential_reauth",
      requested_scope: { lane: "azure_devops", mechanism: "entra_consent", owner: "me", provider_id: "row_1", scopes: [] },
      state: "PENDING",
      requested_at: new Date().toISOString(),
    },
  ]);

// CONSOLE-RULES §5: every actor on a row is two adjacent glyphs, never fused —
// WHO (the agent monogram) and WHAT (the state). The state's WORD moved to
// row 2, but it stays in the DOM: the e2e suite reads state off that text.
describe("RunCard — two-row anatomy", () => {
  it("carries WHO and WHAT as separate glyphs, and the state word on row 2", () => {
    renderCard(run({ state: "WAITING_FOR_CONFIRMATION" }));
    // WHO — the monogram, not fused into the state.
    expect(screen.getByText("CC")).toBeInTheDocument();
    // WHAT — its own labelled glyph.
    expect(screen.getByRole("img", { name: "Needs you" })).toBeInTheDocument();
    // …and the word the e2e suite asserts.
    expect(screen.getByText("Awaiting confirmation")).toBeInTheDocument();
  });

  // W6-U SHOULD-1 — the board shows an admin every run, and a member the
  // shared-lane rows their own runs raised, so "your AWS sign-in" was false on
  // the very cards a support scenario opens. Only the owner's sign-in clears
  // the hold.
  it("a run held on an AWS sign-in names whose sign-in, by the reader", () => {
    renderCard(run(), reauthSignals("run_3b7f10c4aa99"), "me");
    expect(screen.getByText(waitingReauth(1))).toBeInTheDocument();
  });

  it("…and the same card read by somebody else says the owner's", () => {
    renderCard(run(), reauthSignals("run_3b7f10c4aa99"), "admin@corp");
    expect(screen.getByText(waitingReauth(1, false))).toBeInTheDocument();
    expect(screen.queryByText(waitingReauth(1))).not.toBeInTheDocument();
  });

  // S10 round 2 (F13) — the Azure DevOps consent chip must never say "AWS".
  it("a run held on an Azure DevOps consent request names Azure DevOps, never AWS, by the reader", () => {
    renderCard(run(), adoConsentSignals("run_3b7f10c4aa99"), "me");
    expect(screen.getByText(waitingAdoConsent(1))).toBeInTheDocument();
    expect(screen.queryByText(waitingReauth(1))).not.toBeInTheDocument();
    expect(screen.queryByText(/AWS/)).not.toBeInTheDocument();
  });

  // A mid-run Azure DevOps SIGN-IN request is the same chip, never the AWS one.
  it("a run held on an Azure DevOps sign-in request names Azure DevOps, never AWS", () => {
    const signals = approvalSignals([
      {
        id: "a-ado-signin",
        run_id: "run_3b7f10c4aa99",
        kind: "credential_reauth",
        requested_scope: { lane: "azure_devops", mechanism: "entra_signin", reason: "signin", owner: "me", provider_id: "row_1" },
        state: "PENDING",
        requested_at: new Date().toISOString(),
      },
    ]);
    renderCard(run(), signals, "me");
    expect(screen.getByText(waitingAdoConsent(1))).toBeInTheDocument();
    expect(screen.queryByText(/AWS/)).not.toBeInTheDocument();
  });

  it("…and the same card read by somebody else says the owner's Azure DevOps sign-in", () => {
    renderCard(run(), adoConsentSignals("run_3b7f10c4aa99"), "admin@corp");
    expect(screen.getByText(waitingAdoConsent(1, false))).toBeInTheDocument();
    expect(screen.queryByText(waitingAdoConsent(1))).not.toBeInTheDocument();
  });

  it("row 2 carries repo, barrier, short id and age", () => {
    renderCard(run());
    expect(screen.getByText("acme/payments-api")).toBeInTheDocument();
    expect(screen.getByText("Vault")).toBeInTheDocument();
    // run_ prefix stripped, clipped with an ellipsis, full id on the title.
    expect(screen.getByTitle("run_3b7f10c4aa99")).toHaveTextContent("3b7f10c4…");
  });

  it("a held approval says what is waiting; a failure adds nothing beyond the glyph and Review", () => {
    const held = approvalSignals([
      {
        id: "a1",
        run_id: "run_3b7f10c4aa99",
        kind: "egress_domain",
        requested_scope: { host: "h", mode: "wait_for_review" },
        state: "PENDING",
        requested_at: new Date().toISOString(),
      },
    ]);
    renderCard(run(), held);
    expect(screen.getByText("1 waiting · sandbox held")).toBeInTheDocument();

    // A held approval or a failure adds nothing beyond the glyph and the
    // Review button — no sentence restates the state.
    renderCard(run({ id: "run-2", state: "FAILED" }));
    expect(screen.queryByText(/Run failed — review what happened/)).toBeNull();
    expect(screen.queryByText(/Waiting for your confirmation/)).toBeNull();
  });

  it("the action is always reachable on a card that needs eyes; Attach is revealed, never hover-only", () => {
    // #215: a failed run is a REPORT, not a request — "Open", not "Review",
    // the word a held run still keeps (see the held-run cases above/below).
    const { unmount } = renderCard(run({ state: "FAILED" }));
    const review = screen.getByRole("button", { name: "Open" });
    expect(review).toBeInTheDocument();
    // …and never teal: §2 keeps the accent for the one `default` button per
    // surface, which on the board is the shell's New run.
    expect(review.className).not.toContain("bg-primary");
    unmount();

    renderCard(run({ interactive: true, state: "RUNNING" }));
    const attach = screen.getByRole("button", { name: /Attach/ });
    // Revealed by hover AND focus-within, so it is reachable from the keyboard.
    expect(attach.className).toContain("group-hover:opacity-100");
    expect(attach.className).toContain("group-focus-within:opacity-100");
  });

  it("the barrier strength strip is gone from the card — the chip already names the tier", () => {
    const { container } = renderCard(run());
    // The strip renders its ladder as a row of filled segments; only the chip
    // and its icon should carry the tier here now.
    expect(screen.getAllByText("Vault")).toHaveLength(1);
    expect(container.querySelectorAll(".bg-vault-fg")).toHaveLength(0);
  });

  // #215 — the card was a div with onClick: no anchor, no role, no tabIndex,
  // so a run could not be reached by keyboard, middle-clicked, or copied as a
  // link. The title is now a real <a href>.
  it("the run title is a real <a href>, reachable by keyboard", () => {
    renderCard(run());
    const link = screen.getByRole("link", { name: "Rotate the staging credentials" });
    expect(link).toHaveAttribute("href", "/runs/run_3b7f10c4aa99");
    link.focus();
    expect(link).toHaveFocus();
  });
});

// #160 — isHeld's stale-hold ceiling (lib/types/approvals.ts): a tool_call or
// credential_reauth row older than 60 minutes stops counting as a live hold.
// This pins the RUNS BOARD call site (approvalSignals -> RunCard); the
// cockpit command bar's call site is pinned in run-detail.test.tsx.
describe("RunCard — a stale hold degrades the card's own claim (#160)", () => {
  const staleToolCall: RunSignals = approvalSignals([
    {
      id: "a1",
      run_id: "run_3b7f10c4aa99",
      kind: "tool_call",
      requested_scope: { tool: "Bash", cmd: "rm -rf build" },
      state: "PENDING",
      requested_at: new Date(Date.now() - 90 * 60_000).toISOString(),
    },
  ]);

  it("offers Open, not Review, once the hold is stale", () => {
    renderCard(run({ state: "WAITING_FOR_CONFIRMATION" }), staleToolCall);
    expect(screen.getByRole("button", { name: "Open" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Review" })).not.toBeInTheDocument();
  });

  it("says a neutral 'was held', not the live warning sentence", () => {
    renderCard(run({ state: "WAITING_FOR_CONFIRMATION" }), staleToolCall);
    expect(screen.getByText("Was held — check the run")).toBeInTheDocument();
    expect(screen.queryByText(/sandbox held/)).not.toBeInTheDocument();
  });

  // The DELIBERATE LIMIT: only the derived claim degrades. The run's own
  // wire state, via RunStateBadge, still reads exactly what it is — restyling
  // it would invent a new tone for a state that has not changed.
  it("leaves RunStateBadge alone — the wire state still reads Awaiting confirmation", () => {
    renderCard(run({ state: "WAITING_FOR_CONFIRMATION" }), staleToolCall);
    expect(screen.getByText("Awaiting confirmation")).toBeInTheDocument();
  });

  it("a FRESH tool_call hold (same kind, well inside the ceiling) still says Review and the live sentence", () => {
    const fresh = approvalSignals([
      {
        id: "a1",
        run_id: "run_3b7f10c4aa99",
        kind: "tool_call",
        requested_scope: { tool: "Bash", cmd: "rm -rf build" },
        state: "PENDING",
        requested_at: new Date().toISOString(),
      },
    ]);
    renderCard(run({ state: "WAITING_FOR_CONFIRMATION" }), fresh);
    expect(screen.getByRole("button", { name: "Review" })).toBeInTheDocument();
    expect(screen.getByText("1 waiting · sandbox held")).toBeInTheDocument();
  });
});

// 0.7.3 F7 — the Runs-list door onto the same clone the run header offers.
describe("RunCard — kebab clone door", () => {
  // ticket: 0.7.3 F7
  async function openMenu() {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: "Run actions" }));
    return user;
  }

  it("a terminal run's kebab offers the clone door", async () => {
    renderCard(run({ state: "COMPLETED" }));
    await openMenu();
    expect(screen.getByRole("menuitem", { name: RUN.CLONE_CTA })).toBeInTheDocument();
  });

  it("a live run's kebab does not", async () => {
    renderCard(run({ state: "RUNNING" }));
    await openMenu();
    expect(screen.queryByRole("menuitem", { name: RUN.CLONE_CTA })).toBeNull();
  });
});

// review C-01/C-06/C-07 — cloneRun's own behaviour: the happy path carries an
// audit-derived field through to the prefill, a rejected fetch toasts and
// never navigates, and an EMPTY read (an older run / pruned trail / a
// non-owner's empty 200) refuses rather than launching a defaults-degraded
// clone.
describe("RunCard — cloneRun behaviour (review C-01/C-06/C-07)", () => {
  afterEach(() => {
    listAuditMock.mockReset();
    navigateMock.mockReset();
    vi.mocked(toast.error).mockClear();
    vi.mocked(toast.warning).mockClear();
  });

  const createEvent = (data: Record<string, unknown>): AuditEvent => ({
    id: "ev-create",
    time: "2026-09-14T10:00:00Z",
    actor_type: "human",
    actor: "alice",
    action: "run.create",
    outcome: "success",
    data,
  });

  async function clickClone() {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: "Run actions" }));
    await user.click(await screen.findByRole("menuitem", { name: RUN.CLONE_CTA }));
  }

  it("happy path: navigates with an audit-derived field (tool_approvals) in the prefill", async () => {
    listAuditMock.mockResolvedValue([createEvent({ tool_approvals: "hold" })]);
    renderCard(run({ state: "COMPLETED" }));
    await clickClone();

    await waitFor(() => expect(navigateMock).toHaveBeenCalled());
    expect(listAuditMock).toHaveBeenCalledWith("run_3b7f10c4aa99", "run.create");
    const [path, opts] = navigateMock.mock.calls[0];
    expect(path).toBe("/runs/new");
    expect(opts.state.prefill.state.toolApprovals).toBe("hold");
  });

  // The refusal sentence is a DRAFT constant beside CLONE_UNREADABLE, not a
  // literal inline in this component — the two clone doors' OTHER string was
  // hoisted for exactly that reason (U-01, blind round 2, lens-U2).
  it("a rejected fetch toasts CLONE_LOAD_FAILED and never navigates", async () => {
    listAuditMock.mockRejectedValue(new Error("network down"));
    renderCard(run({ state: "COMPLETED" }));
    await clickClone();

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith(CLONE_LOAD_FAILED, { description: "network down" }),
    );
    expect(navigateMock).not.toHaveBeenCalled();
  });

  it("an empty read refuses instead of launching a defaults-degraded clone", async () => {
    listAuditMock.mockResolvedValue([]);
    renderCard(run({ state: "COMPLETED" }));
    await clickClone();

    await waitFor(() =>
      expect(toast.warning).toHaveBeenCalledWith(
        "This run's launch settings couldn't be read — its clone would start from defaults, so it was not opened.",
      ),
    );
    expect(navigateMock).not.toHaveBeenCalled();
  });
});
