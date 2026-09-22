/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { AdoCapabilityCard } from "./ado-capability-card";

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
    requested_at: "2026-09-22T14:02:11.000Z",
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
    requested_at: "2026-09-22T14:09:52.000Z",
    ...overrides,
  };
}

function renderCard(ui: React.ReactElement) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

describe("AdoCapabilityCard — the escalation states", () => {
  it("shows the capability heading in plain words, the repository and the composed command", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation()}
        operator
        busy={false}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("Push")).toBeInTheDocument();
    expect(within(card).getByText("acme/payments-api")).toBeInTheDocument();
    expect(
      within(card).getByText("Push commits and move branches that no policy protects (code_write) in acme/payments-api"),
    ).toBeInTheDocument();
    // Not protected — no Ref class row.
    expect(within(card).queryByText("Ref class")).not.toBeInTheDocument();
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
        operator
        busy={false}
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
        operator
        busy={false}
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
      <AdoCapabilityCard item={escalation()} operator busy={false} onApprove={onApprove} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("Scope: This run")).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: "Approve" }));
    expect(onApprove).toHaveBeenCalledWith([{ scope: "run" }]);
  });

  it("picking Once from the caret stages it, and Approve/Deny both then send scope once", async () => {
    const onApprove = vi.fn();
    const onDeny = vi.fn();
    renderCard(
      <AdoCapabilityCard item={escalation()} operator busy={false} onApprove={onApprove} onDeny={onDeny} />,
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

  it("Until… and Always are disabled with their refusal reason (ADO has no clock and no workspace to write to)", async () => {
    renderCard(<AdoCapabilityCard item={escalation()} operator busy={false} onApprove={vi.fn()} onDeny={vi.fn()} />);
    const card = await screen.findByTestId("ado-capability-card");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(card).getByRole("button", { name: /more options/i }));
    expect(await screen.findByText(/can't outlive the run/)).toBeInTheDocument();
    expect(screen.getByText(/nothing here is saved to the workspace/)).toBeInTheDocument();
  });

  it("renders no buttons and 'Not yours to decide' for a non-owner, non-operator viewer", async () => {
    const onApprove = vi.fn();
    renderCard(
      <AdoCapabilityCard
        item={escalation()}
        operator={false}
        runOwner="dana@acme.example"
        viewerPrincipal="priya@acme.example"
        busy={false}
        onApprove={onApprove}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("Not yours to decide")).toBeInTheDocument();
    expect(within(card).getByText(/Only dana@acme.example, who started this run, or an admin can answer this\./)).toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });

  it("is decidable when the viewer IS the run's owner, with no operator tier at all", async () => {
    renderCard(
      <AdoCapabilityCard
        item={escalation()}
        operator={false}
        runOwner="dana@acme.example"
        viewerPrincipal="dana@acme.example"
        busy={false}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  it("shows the run-ended sentence and no controls once the run has ended", async () => {
    renderCard(
      <AdoCapabilityCard item={escalation()} operator runEnded busy={false} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText(/The run ended before anyone decided this/)).toBeInTheDocument();
    expect(within(card).queryByRole("button")).not.toBeInTheDocument();
  });
});

describe("AdoCapabilityCard — the Entra-consent state", () => {
  it("the owner sees the consent chip and the 'Allow and continue' door, to Settings", async () => {
    renderCard(
      <AdoCapabilityCard
        item={consent()}
        operator={false}
        viewerPrincipal="dana@acme.example"
        busy={false}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-consent-card");
    expect(within(card).getByText("Needs your Microsoft consent")).toBeInTheDocument();
    const cta = within(card).getByRole("link", { name: "Allow and continue" });
    expect(cta).toHaveAttribute("href", "/settings");
  });

  it("someone who is not the owner sees who it's waiting on, and no door", async () => {
    renderCard(
      <AdoCapabilityCard
        item={consent({ requested_scope: { ...consent().requested_scope, owner: "dana@acme.example" } as never })}
        operator={false}
        viewerPrincipal="priya@acme.example"
        busy={false}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = await screen.findByTestId("ado-consent-card");
    expect(within(card).getByText("Waiting for dana@acme.example")).toBeInTheDocument();
    expect(within(card).getByText(/Only dana@acme.example can give Microsoft/)).toBeInTheDocument();
    expect(within(card).queryByRole("link")).not.toBeInTheDocument();
  });
});
