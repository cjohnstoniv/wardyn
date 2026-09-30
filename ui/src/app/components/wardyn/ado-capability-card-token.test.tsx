/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The approval card's one added line (mock state 7): on a run whose Azure DevOps
// token Wardyn creates, approving replaces that token with a wider one, even
// when the approval is for one request only.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { ADO_PAT } from "../../lib/ado-pat-copy";
import { AdoCapabilityCard } from "./ado-capability-card";

const item: ApprovalRequest = {
  id: "apr_1",
  run_id: "run_1",
  grant_id: "grant_1",
  kind: "tool_call",
  requested_scope: {
    lane: "azure_devops",
    provider_id: "row_1",
    org: "acme",
    grant_id: "grant_1",
    capability: "pr",
    repo: "payments-api",
    ref_class: "",
    tool: "Azure DevOps",
    cmd: "Open a pull request",
  },
  state: "PENDING",
  requested_at: new Date().toISOString(),
};

function draw(tokenWidens?: boolean) {
  render(
    <MemoryRouter>
      <AdoCapabilityCard
        item={item}
        securityOperator
        run={{ created_by: "dana@acme.example", state: "RUNNING" }}
        tokenWidens={tokenWidens}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />
    </MemoryRouter>,
  );
}

describe("AdoCapabilityCard on a run whose token Wardyn creates", () => {
  it("says approving adds the access to the run's token for the rest of the run, even for a one-time approval", async () => {
    draw(true);
    expect(await screen.findByText(ADO_PAT.APPROVAL_WIDENS)).toBeInTheDocument();
  });

  it("says nothing of a token otherwise", async () => {
    draw(false);
    await screen.findByTestId("ado-capability-card");
    expect(screen.queryByText(ADO_PAT.APPROVAL_WIDENS)).not.toBeInTheDocument();
  });

  it("says nothing when the caller does not say (the sign-in lane, or an unread run)", async () => {
    draw();
    await screen.findByTestId("ado-capability-card");
    expect(screen.queryByText(ADO_PAT.APPROVAL_WIDENS)).not.toBeInTheDocument();
  });
});
