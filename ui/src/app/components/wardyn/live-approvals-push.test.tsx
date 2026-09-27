/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #181 — the held-push card on the run cockpit's live strip. Its own file,
// the same split live-approvals-ado.test.tsx/live-approvals-reauth.test.tsx
// already use, rather than growing live-approvals.test.tsx further.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ApprovalRequest } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { SECURITY_ONLY_REASON } from "./copy";
import { PUSH } from "./copy/push";

const listApprovalsMock = vi.fn((..._a: unknown[]): Promise<ApprovalRequest[]> => Promise.resolve([]));
const approveMock = vi.fn((..._a: unknown[]): Promise<unknown> => Promise.resolve({}));
const denyMock = vi.fn((..._a: unknown[]): Promise<unknown> => Promise.resolve({}));
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: (...a: unknown[]) => denyMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { LiveApprovals } from "./live-approvals";

function pushRow(over: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "apr_push_1",
    run_id: "r1",
    kind: "push_content",
    requested_scope: {
      repo: "github.com/acme/widgets",
      branch: "refs/heads/feature/x",
      acts_as: "github_token:11111111-1111-1111-1111-111111111111",
      paths: ["a.txt"],
      paths_total: 1,
      commits: ["deadbeef".repeat(5)],
      paths_digest: "a".repeat(64),
      acts_as_kind: "github_app",
      acts_as_label: "dana@acme.example",
    },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  } as ApprovalRequest;
}

describe("LiveApprovals — a held push (#181)", () => {
  beforeEach(() => {
    listApprovalsMock.mockReset().mockResolvedValue([pushRow()]);
    approveMock.mockReset().mockResolvedValue({});
    denyMock.mockReset().mockResolvedValue({});
  });

  it("renders the full push card on the strip (not the generic one-liner row) and decides with no scope args", async () => {
    render(<LiveApprovals runId="r1" />);
    const card = await screen.findByTestId("push-content-card");
    expect(within(card).getByText(PUSH.CARD_TITLE)).toBeInTheDocument();
    expect(within(card).getByText("github.com/acme/widgets")).toBeInTheDocument();
    within(card).getByRole("button", { name: /^Approve$/ }).click();
    await screen.findByTestId("live-approvals"); // still mounted; decide resolved
    expect(approveMock).toHaveBeenCalledWith("apr_push_1", "approved live");
  });

  it("a member sees the card and the admin-only hint, with no Approve/Deny", async () => {
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <LiveApprovals runId="r1" />
      </OperatorProvider>,
    );
    const card = await screen.findByTestId("push-content-card");
    expect(within(card).queryByRole("button", { name: /^Approve$/ })).not.toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: /^Deny$/ })).not.toBeInTheDocument();
    expect(within(card).getByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
  });
});
