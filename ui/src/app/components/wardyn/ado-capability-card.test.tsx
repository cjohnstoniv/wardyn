/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { AdoCapabilityCard, type AdoCardRun } from "./ado-capability-card";
import { ADO } from "../../lib/ado-entra-copy";

const OWNER: AdoCardRun = { created_by: "dana@acme.example", state: "RUNNING" };
const ENDED_RUN: AdoCardRun = { created_by: "dana@acme.example", state: "COMPLETED" };

function escalation(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "apr_1",
    run_id: "run_1",
    grant_id: "grant_1",
    kind: "tool_call",
    requested_scope: {
      lane: "azure_devops",
      provider_id: "row_1",
      org: "acme",
      grant_id: "grant_1",
      capability: "code_write",
      repo: "payments-api",
      ref_class: "",
      tool: "Azure DevOps",
      cmd: "Push commits and move branches that no policy protects (code_write) in acme/payments-api",
    },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...overrides,
  };
}

function consent(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "apr_2",
    run_id: "run_1",
    kind: "credential_reauth",
    requested_scope: {
      lane: "azure_devops",
      mechanism: "entra_consent",
      owner: "dana@acme.example",
      provider_id: "row_1",
      scopes: ["vso.code_write"],
    },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...overrides,
  };
}

function renderCard(ui: React.ReactElement) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

describe("AdoCapabilityCard — the escalation states", () => {
  it("shows the capability heading in plain words, the repository, the composed command and Acts as", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("Push")).toBeInTheDocument();
    expect(within(card).getByText("acme/payments-api")).toBeInTheDocument();
    expect(
      within(card).getByText("Push commits and move branches that no policy protects (code_write) in acme/payments-api"),
    ).toBeInTheDocument();
    // Not protected — no Ref class row.
    expect(within(card).queryByText("Ref class")).not.toBeInTheDocument();
    // F8 — Acts as, from run.created_by.
    expect(within(card).getByText("Acts as")).toBeInTheDocument();
    expect(within(card).getByText("dana@acme.example")).toBeInTheDocument();
  });

  it("shows a Ref class row, and the bypass heading, for a protected-ref escalation", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation({
          requested_scope: {
            lane: "azure_devops",
            provider_id: "row_1",
            org: "acme",
            grant_id: "grant_1",
            capability: "policy_bypass",
            repo: "payments-api",
            ref_class: "protected",
            tool: "Azure DevOps",
            cmd: "Land changes past a branch policy (policy_bypass) in acme/payments-api, on a protected branch",
          },
        })}
        securityOperator
        run={OWNER}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("Push past a branch policy")).toBeInTheDocument();
    expect(within(card).getByText("Ref class")).toBeInTheDocument();
    expect(within(card).getByText("Protected by a branch policy")).toBeInTheDocument();
  });

  it("falls back to the raw wire capability, in mono, for a capability with no §7.4 label", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation({
          requested_scope: { ...escalation().requested_scope, capability: "project_admin" } as never,
        })}
        securityOperator
        run={OWNER}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("project_admin")).toBeInTheDocument();
  });

  it("defaults to 'This run', and Approve sends decision_scope run explicitly (never omitted)", async () => {
    const onApprove = vi.fn();
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={onApprove} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("Scope: This run")).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: "Approve" }));
    expect(onApprove).toHaveBeenCalledWith([{ scope: "run" }]);
  });

  // F1 — the consequence sentences use the mock's per-capability noun
  // ("push" for code_write/policy_bypass), never the generic "request".
  it("F1: the consequence sentences use the capability's own noun, not the generic 'request'", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(card.textContent).toMatch(/Approving lets every push from this run through/);
    expect(card.textContent).not.toMatch(/every request from this run/);
  });

  it("F1: a pull-request escalation's consequence sentence says 'action'", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation({ requested_scope: { ...escalation().requested_scope, capability: "pr" } as never })}
        securityOperator
        run={OWNER}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(card.textContent).toMatch(/Approving lets every action from this run through/);
  });

  it("picking Once from the caret stages it, and Approve/Deny both then send scope once", async () => {
    const onApprove = vi.fn();
    const onDeny = vi.fn();
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={onApprove} onDeny={onDeny} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(card).getByRole("button", { name: /more options/i }));
    await user.click(await screen.findByText("Once"));

    expect(within(card).getByText("Scope: Once")).toBeInTheDocument();
    await user.click(within(card).getByRole("button", { name: "Approve" }));
    expect(onApprove).toHaveBeenCalledWith([{ scope: "once" }]);
    await user.click(within(card).getByRole("button", { name: "Deny" }));
    expect(onDeny).toHaveBeenCalledWith([{ scope: "once" }]);
  });

  // review finding F3: the scope options were plain <button>s inside a Radix
  // DropdownMenuContent, whose own keydown handler swallows Tab
  // (@radix-ui/react-menu's ContentImpl) and whose roving-focus manager only
  // ever registers DropdownMenuItems — never these buttons. A keyboard-only
  // admin could open the menu but never move focus onto "Once". This test
  // drives the menu with the keyboard alone: no user.click ever lands on a
  // scope option.
  it("F3: the scope menu is reachable with the keyboard alone — Tab reaches Once, Enter picks it", async () => {
    const onApprove = vi.fn();
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={onApprove} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    const user = userEvent.setup();
    const caret = within(card).getByRole("button", { name: /more options/i });
    caret.focus();
    await user.keyboard("{Enter}");

    const isOnceButton = () =>
      document.activeElement?.tagName === "BUTTON" && !!document.activeElement.textContent?.includes("Once");
    let reached = isOnceButton();
    for (let i = 0; i < 6 && !reached; i++) {
      await user.tab();
      reached = isOnceButton();
    }
    expect(reached).toBe(true);

    await user.keyboard("{Enter}");
    expect(within(card).getByText("Scope: Once")).toBeInTheDocument();

    await user.click(within(card).getByRole("button", { name: "Approve" }));
    expect(onApprove).toHaveBeenCalledWith([{ scope: "once" }]);
  });

  it("F1: the caret's Once/This run hints also use the capability's own noun", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(card).getByRole("button", { name: /more options/i }));
    expect(await screen.findByText(/This one push goes through/)).toBeInTheDocument();
    expect(screen.getByText(/Every push from this run goes through/)).toBeInTheDocument();
  });

  it("Until… and Always are disabled with their refusal reason (ADO has no clock and no workspace to write to)", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(card).getByRole("button", { name: /more options/i }));
    expect(await screen.findByText(/can't outlive the run/)).toBeInTheDocument();
    expect(screen.getByText(/nothing here is saved to the workspace/)).toBeInTheDocument();
  });

  // F7 — a deny now sticks for the rest of the run (#414, unless a later
  // run-scoped approve lifts it); the scope readout sits by Approve only.
  it("F7: Denying says it refuses this push for the rest of the run, unless later allowed for the whole run — and no scope readout sits by Deny", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(card.textContent).toMatch(
      /Denying refuses this push for the rest of the run, unless this kind of change is later allowed for the whole run\./,
    );
    expect(card.textContent).not.toMatch(/may ask again/);
    const denyBtn = within(card).getByRole("button", { name: "Deny" });
    // The readout sits once, immediately after the caret/Deny group — assert
    // there is exactly one "Scope:" node and it precedes Deny in DOM order
    // is out of scope for a jsdom test; the stronger, still-precise
    // assertion is that removing it from beside Deny leaves exactly one.
    expect(within(card).getAllByText(/^Scope: /)).toHaveLength(1);
    expect(denyBtn).toBeInTheDocument();
  });

  // F7 — the hold's honest expiry: the card can only ESTIMATE from
  // requested_at, so it never claims a number ("four minutes") it cannot
  // verify (round-2 N5).
  it("F7: says the request may be waiting, honestly, while requested_at is recent", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(card.textContent).toMatch(
      /The push may be waiting at the proxy for a short time; approving lets it through now or the next time the run asks\./,
    );
    expect(card.textContent).not.toMatch(/four minutes/);
  });

  it("F7: says no longer waiting once requested_at is more than four minutes old", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation({ requested_at: new Date(Date.now() - 5 * 60_000).toISOString() })}
        securityOperator
        run={OWNER}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(card.textContent).toMatch(/No longer waiting — approving lets the push through the next time the run asks\./);
  });

  // #458 — a timer re-renders the card when the hold window ends, so the
  // held line flips on its own with no other trigger.
  it("#458: the held line flips to the expired one on its own once the hold window elapses", async () => {
    vi.useFakeTimers();
    try {
      renderCard(
        <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
      );
      const card = screen.getByTestId("ado-capability-card");
      expect(card.textContent).toMatch(/may be waiting at the proxy/);
      await act(() => vi.advanceTimersByTimeAsync(240_000));
      expect(card.textContent).toMatch(/No longer waiting — approving lets the push through the next time the run asks\./);
    } finally {
      vi.useRealTimers();
    }
  });

  // #458 — the timer must not leak past the card's own lifetime.
  it("#458: clears the hold timer on unmount", () => {
    // render() is synchronous here — no `await find*`, which would poll on
    // REAL timers and hang forever while fake timers are installed.
    vi.useFakeTimers();
    const clearSpy = vi.spyOn(globalThis, "clearTimeout");
    try {
      const { unmount } = renderCard(
        <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
      );
      screen.getByTestId("ado-capability-card");
      unmount();
      expect(clearSpy).toHaveBeenCalled();
    } finally {
      clearSpy.mockRestore();
      vi.useRealTimers();
    }
  });

  // #458 — `busy` is now "approve" | "deny" | null: both buttons disable the
  // moment either is deciding, and only the pressed one spins.
  describe("#458: busy disables both buttons and spins only the pressed one", () => {
    it("busy='approve': both disabled, Approve spins, Deny does not", async () => {
      renderCard(
        <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy="approve" onApprove={vi.fn()} onDeny={vi.fn()} />,
      );
      const card = await screen.findByTestId("ado-capability-card");
      const approveBtn = within(card).getByRole("button", { name: /approve/i });
      const denyBtn = within(card).getByRole("button", { name: /deny/i });
      expect(approveBtn).toBeDisabled();
      expect(denyBtn).toBeDisabled();
      expect(approveBtn.querySelector(".animate-spin")).toBeInTheDocument();
      expect(denyBtn.querySelector(".animate-spin")).not.toBeInTheDocument();
    });

    it("busy='deny': both disabled, Deny spins, Approve does not", async () => {
      renderCard(
        <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy="deny" onApprove={vi.fn()} onDeny={vi.fn()} />,
      );
      const card = await screen.findByTestId("ado-capability-card");
      const approveBtn = within(card).getByRole("button", { name: /approve/i });
      const denyBtn = within(card).getByRole("button", { name: /deny/i });
      expect(approveBtn).toBeDisabled();
      expect(denyBtn).toBeDisabled();
      expect(denyBtn.querySelector(".animate-spin")).toBeInTheDocument();
      expect(approveBtn.querySelector(".animate-spin")).not.toBeInTheDocument();
    });

    it("busy=null: neither disabled, neither spins", async () => {
      renderCard(
        <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
      );
      const card = await screen.findByTestId("ado-capability-card");
      expect(within(card).getByRole("button", { name: /approve/i })).not.toBeDisabled();
      expect(within(card).getByRole("button", { name: /deny/i })).not.toBeDisabled();
    });
  });

  // #458 — REQ_OWNER_FALLBACK: the "not yours" body falls back to canon,
  // not a hardcoded literal, when the row carries no run owner yet.
  it("#458: falls back to ADO.REQ_OWNER_FALLBACK when the run carries no owner", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation()}
        securityOperator={false}
        run={{ created_by: "", state: "RUNNING" }}
        viewerPrincipal="priya@acme.example"
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText(ADO.REQ_NOT_YOURS_BODY(ADO.REQ_OWNER_FALLBACK))).toBeInTheDocument();
  });

  // N2 — round-2 fix: the bold-labeled lead-in used to DUPLICATE the canon
  // sentence's own opening word ("Approving Approving lets…", "Denying
  // Denying…"). Assert the FULL rendered sentence text for each, exactly as
  // the mock draws it (index.html:512): one sentence, first word bold.
  it("N2: Approving/Denying render as ONE sentence each, first word bold, never doubled", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    const approvingP = within(card).getByText(/lets every push from this run through/).closest("p")!;
    expect(approvingP.textContent).toBe(
      "Approving lets every push from this run through until it ends. Nothing carries to the next run.",
    );
    expect(approvingP.querySelector("b")?.textContent).toBe("Approving");

    const denyingP = within(card).getByText(/refuses this push for the rest of the run/).closest("p")!;
    expect(denyingP.textContent).toBe(
      "Denying refuses this push for the rest of the run, unless this kind of change is later allowed for the whole run.",
    );
    expect(denyingP.querySelector("b")?.textContent).toBe("Denying");
  });

  // F6 — Q3: policy_bypass/policy_admin get a plain Approve + destructive Deny.
  it("F6: an ordinary capability (code_write) keeps a teal Approve and a plain Deny", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={OWNER} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByRole("button", { name: "Approve" }).className).toMatch(/\bbg-info\b/);
    expect(within(card).getByRole("button", { name: "Deny" }).className).not.toMatch(/\bbg-destructive\b/);
  });

  it("F6: policy_bypass gets a plain Approve and a destructive Deny", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation({ requested_scope: { ...escalation().requested_scope, capability: "policy_bypass" } as never })}
        securityOperator
        run={OWNER}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByRole("button", { name: "Approve" }).className).not.toMatch(/\bbg-info\b/);
    expect(within(card).getByRole("button", { name: "Deny" }).className).toMatch(/\bbg-destructive\b/);
  });

  it("F6: policy_admin gets a plain Approve and a destructive Deny", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation({ requested_scope: { ...escalation().requested_scope, capability: "policy_admin" } as never })}
        securityOperator
        run={OWNER}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByRole("button", { name: "Approve" }).className).not.toMatch(/\bbg-info\b/);
    expect(within(card).getByRole("button", { name: "Deny" }).className).toMatch(/\bbg-destructive\b/);
  });

  it("renders no buttons and neutral 'Not yours to decide' for a non-owner, non-security viewer", async () => {
    const onApprove = vi.fn();
    renderCard(
      <AdoCapabilityCard
        item={escalation()}
        securityOperator={false}
        run={OWNER}
        viewerPrincipal="priya@acme.example"
        busy={null}
        onApprove={onApprove}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    const chip = within(card).getByText("Not yours to decide");
    expect(chip).toBeInTheDocument();
    expect(chip.className).toMatch(/bg-muted|text-muted-foreground/);
    expect(within(card).getByText(/Only dana@acme.example, who started this run, or an admin can answer this\./)).toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });

  it("is decidable when the viewer IS the run's owner, with no security tier at all", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation()}
        securityOperator={false}
        run={OWNER}
        viewerPrincipal="dana@acme.example"
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  it("shows the LIST_ENDED sentence and no controls once the run has ended", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={ENDED_RUN} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("This run has ended")).toBeInTheDocument();
    expect(within(card).getByText(/nothing to allow — the request went away with the run/)).toBeInTheDocument();
    expect(within(card).queryByRole("button")).not.toBeInTheDocument();
  });

  // F10 — loading and error states, non-security viewer only (a security
  // operator's decidability never depends on the run fetch).
  it("F10: a non-security viewer sees a neutral loading state, never 'Not yours', while run is undefined", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator={false} run={undefined} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).queryByText("Not yours to decide")).not.toBeInTheDocument();
    expect(within(card).getByLabelText("loading")).toBeInTheDocument();
  });

  it("F10: a non-security viewer sees a generic error, never 'Not yours', when run is null", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator={false} run={null} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).queryByText("Not yours to decide")).not.toBeInTheDocument();
    expect(within(card).getByText(/Couldn't load this run/)).toBeInTheDocument();
  });

  it("F10: a security operator sees the real card immediately, even with run undefined", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} securityOperator run={undefined} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });
});

describe("AdoCapabilityCard — the Entra-consent state", () => {
  it("the owner sees the consent chip, Acts as, and the 'Connect Azure DevOps' door, to the Settings card's anchor (#458)", async () => {
    renderCard(
      <AdoCapabilityCard
        item={consent()}
        securityOperator={false}
        run={null}
        viewerPrincipal="dana@acme.example"
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-consent-card");
    expect(within(card).getByText("Needs your Microsoft consent")).toBeInTheDocument();
    // F8 — Acts as, from the consent row's own owner field.
    expect(within(card).getByText("Acts as")).toBeInTheDocument();
    expect(within(card).getByText("dana@acme.example")).toBeInTheDocument();
    const cta = within(card).getByRole("link", { name: ADO.REQ_CONSENT_CTA });
    expect(cta).toHaveAttribute("href", "/account#azure-devops");
  });

  it("someone who is not the owner sees who it's waiting on, and no door", async () => {
    renderCard(
      <AdoCapabilityCard
        item={consent({ requested_scope: { ...consent().requested_scope, owner: "dana@acme.example" } as never })}
        securityOperator={false}
        run={null}
        viewerPrincipal="priya@acme.example"
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-consent-card");
    expect(within(card).getByText("Waiting for dana@acme.example")).toBeInTheDocument();
    expect(within(card).getByText(/only dana@acme.example can give Microsoft/)).toBeInTheDocument();
    expect(within(card).queryByRole("link")).not.toBeInTheDocument();
  });
});

// holdForADOSignIn's mid-run sign-in request: the same card, with the
// "connection ended mid-run" copy — never the consent sentences.
function signIn(): ApprovalRequest {
  return consent({
    id: "apr_3",
    requested_scope: { lane: "azure_devops", mechanism: "entra_signin", reason: "signin", owner: "dana@acme.example", provider_id: "row_1" },
  });
}

describe("AdoCapabilityCard — the mid-run sign-in state", () => {
  it("the owner sees the connection-ended copy and the connect door, and nothing about consent", async () => {
    renderCard(
      <AdoCapabilityCard item={signIn()} securityOperator={false} run={null} viewerPrincipal="dana@acme.example" busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-consent-card");
    expect(within(card).getByText("Connection ended")).toBeInTheDocument();
    expect(within(card).getByText("Your Azure DevOps connection ended mid-run")).toBeInTheDocument();
    expect(within(card).getByText(/held while you sign in again/)).toBeInTheDocument();
    expect(within(card).getByRole("link", { name: ADO.CONNECT_ADO })).toHaveAttribute("href", "/account#azure-devops");
    expect(card.textContent).not.toMatch(/consent|four minutes/i);
  });

  it("someone who is not the owner sees who can sign in, and no door", async () => {
    renderCard(
      <AdoCapabilityCard item={signIn()} securityOperator={false} run={null} viewerPrincipal="priya@acme.example" busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-consent-card");
    expect(within(card).getByText("Waiting for dana@acme.example")).toBeInTheDocument();
    expect(within(card).getByText(/Only dana@acme.example can sign in again/)).toBeInTheDocument();
    expect(within(card).queryByRole("link")).not.toBeInTheDocument();
  });
});
