/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities } from "../../lib/types";

// HIGH fix (error handling): approve/deny were unguarded awaits. A rejected
// deny() must NOT leave the dialog's confirm button spinning forever, must
// surface a toast.error, and must keep the dialog open (so the operator can
// retry or cancel). These tests pin that behavior.

// Mock sonner so we can assert toast.error fires without rendering a real toaster.
const toastError = vi.fn();
const toastSuccess = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastError(...a), success: (...a: unknown[]) => toastSuccess(...a) },
}));

// Hand-rolled api fake. listApprovals returns one pending request of
// mockPendingKind (default "credential"; tests override it to prove the B3
// kind-aware decide gating), or none when mockPendingEmpty is set (the
// empty-state copy test); deny() rejects to simulate a 409 / network
// failure. `mock`-prefixed so vi.mock's hoisting can reference them.
let mockPendingKind: ApprovalRequest["kind"] = "credential";
let mockPendingEmpty = false;
const denyMock = vi.fn();
const approveMock = vi.fn();
vi.mock("../../lib/api/approvals", () => {
  return {
    approvals: {
      listApprovals: (state: string) =>
        Promise.resolve(
          state === "PENDING" && !mockPendingEmpty
            ? [
                {
                  id: "apr_1",
                  run_id: "run_1",
                  kind: mockPendingKind,
                  requested_scope: { host: "api.example.com" },
                  state: "PENDING",
                  requested_at: new Date().toISOString(),
                } satisfies ApprovalRequest,
              ]
            : [],
        ),
      deny: (...a: unknown[]) => denyMock(...a),
      approve: (...a: unknown[]) => approveMock(...a),
    },
  };
});
// GET /me/capabilities backs the member why-denied surface below. Default: an
// empty, unenforced set — which is 0.5's behaviour, so every pre-existing test
// in this file is unaffected by it.
let mockCaps: MeCapabilities = {
  grants: [],
  enforcement: {},
  session_groups: [],
  groups_snapshot_stale: false,
};
vi.mock("../../lib/api/permissions", () => ({
  permissions: { getMyCapabilities: () => Promise.resolve(mockCaps) },
}));

// RunContextRow (redesign) fetches the gated run to inline its context.
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: () =>
      Promise.resolve({
        id: "run_1",
        agent: "claude-code",
        repo: "acme/widgets",
        task: "Fix flaky auth tests",
        confinement_class: "CC2",
        state: "RUNNING",
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider, RoleProvider } from "../wardyn/operator-context";
import { DENIED } from "../../lib/permissions-copy";

describe("ApprovalsScreen — deny error handling", () => {
  beforeEach(() => {
    mockPendingKind = "credential";
    toastError.mockClear();
    toastSuccess.mockClear();
    denyMock.mockReset();
    approveMock.mockReset();
  });

  it("surfaces a toast and re-enables the confirm button when deny() rejects", async () => {
    denyMock.mockRejectedValue(new Error("HTTP 409: already decided"));
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );

    // Wait for the pending card to render, then open the deny dialog.
    const denyBtn = await screen.findByRole("button", { name: /deny/i });
    fireEvent.click(denyBtn);

    // The dialog requires a reason for a deny; fill it and confirm.
    const reason = await screen.findByPlaceholderText(/not on allowlist/i);
    fireEvent.change(reason, { target: { value: "bad scope" } });
    const confirm = await screen.findByRole("button", { name: /confirm deny/i });
    fireEvent.click(confirm);

    // toast.error must fire after the rejection.
    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    // The dialog must stay open (confirm button still present) and re-enabled —
    // not stuck spinning forever.
    const confirmAfter = await screen.findByRole("button", { name: /confirm deny/i });
    await waitFor(() => expect(confirmAfter).not.toBeDisabled());
    expect(denyMock).toHaveBeenCalledWith("apr_1", "bad scope");
  });
});

// Role-aware console: the queue itself stays visible to a viewer — only
// deciding is out of reach (see http.go's requireOperator comment).
describe("ApprovalsScreen — role-aware decide buttons", () => {
  beforeEach(() => {
    mockPendingKind = "credential";
    mockPendingEmpty = false;
    denyMock.mockReset();
    approveMock.mockReset();
  });

  it("operator (today's default): Approve and Deny are enabled, no reason shown", async () => {
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).not.toBeDisabled();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
  });

  it("viewer: Approve and Deny are disabled and the reason is visible — the queue itself still renders", async () => {
    render(
      <OperatorProvider operator={false}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    // The request is still readable — a viewer isn't blinded. (scope has no
    // host/repos/key discriminator, so deriveTitle's generic-credential title
    // is the deterministic, exact text to assert on.)
    expect(await screen.findByText("Mint a scoped credential")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    expect(screen.getByText(/requires the admin role/i)).toBeInTheDocument();
    // Clicking a disabled button must never reach the API.
    expect(approveMock).not.toHaveBeenCalled();
    expect(denyMock).not.toHaveBeenCalled();
  });

  // B3: decide() (approvals.go) makes egress_domain owner-or-admin — a member
  // (operator:false) may decide one raised by a run they own; this list is
  // already scoped to the caller's own runs (handleListApprovals), so every
  // row rendered here is, by construction, one the member owns.
  it("member, egress_domain kind: Approve and Deny are LIVE (decide buttons live — prompt-v2)", async () => {
    mockPendingKind = "egress_domain";
    render(
      <OperatorProvider operator={false}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).not.toBeDisabled();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
  });

  // Same member, but the row is credential/tool_call: stays admin-only
  // REGARDLESS of ownership (a self-approved credential mint / a re-opened
  // tool_call would self-authorize under the operator's own ceiling).
  it("member, tool_call kind: still disabled — kind, not just ownership, gates the decision", async () => {
    mockPendingKind = "tool_call";
    render(
      <OperatorProvider operator={false}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    expect(screen.getByText(/requires the admin role/i)).toBeInTheDocument();
  });
});

// prompt-v2 point 5's exact empty-state string, member-only (admin keeps
// "You're all caught up" — the two roles see a genuinely different list, so
// the copy should say so rather than share admin's self-congratulatory tone).
describe("ApprovalsScreen — empty-state copy by role", () => {
  beforeEach(() => {
    mockPendingEmpty = true;
  });

  it("member: \"Approvals raised by your runs appear here.\"", async () => {
    render(
      <RoleProvider role="member">
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </RoleProvider>,
    );
    expect(await screen.findByText("Approvals raised by your runs appear here.")).toBeInTheDocument();
    expect(screen.queryByText("You're all caught up")).not.toBeInTheDocument();
  });

  it("admin (and the fail-open default): unchanged", async () => {
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByText("You're all caught up")).toBeInTheDocument();
  });
});


// 0.6 pillar 2: the ONE member why-denied moment on this screen. A member may
// decide an egress_domain approval on their own run — unless `egress_host` is
// enforced and the host isn't granted to them (authorizeMemberDecision). The
// copy is read from the canon, never retyped.
describe("ApprovalsScreen — egress host not granted (member)", () => {
  beforeEach(() => {
    mockPendingKind = "egress_domain";
    mockPendingEmpty = false;
    denyMock.mockReset();
    approveMock.mockReset();
    mockCaps = { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false };
  });

  function renderMember() {
    return render(
      <OperatorProvider operator={false}>
        <RoleProvider role="member">
          <MemoryRouter>
            <ApprovalsScreen />
          </MemoryRouter>
        </RoleProvider>
      </OperatorProvider>,
    );
  }

  it("unenforced: the member decides their own egress approval, exactly as in 0.5", async () => {
    renderMember();
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.queryByText(DENIED.APPROVE_CHIP)).not.toBeInTheDocument();
  });

  it("enforced + ungranted: both decisions disable, with the reason and the Always caveat", async () => {
    mockCaps = { ...mockCaps, enforcement: { egress_host: true } };
    renderMember();
    expect(await screen.findByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    expect(screen.getByText(DENIED.APPROVE_CHIP)).toBeInTheDocument();
    expect(screen.getByText(DENIED.APPROVE_BODY("api.example.com"))).toBeInTheDocument();
    expect(screen.getByText(DENIED.ALWAYS_STILL_ADMIN)).toBeInTheDocument();
  });

  it("enforced + granted by a *.suffix row: the decision is offered again", async () => {
    mockCaps = {
      ...mockCaps,
      enforcement: { egress_host: true },
      grants: [
        {
          id: "g1",
          subject_type: "user",
          subject: "bob@corp.example",
          capability: "egress_host",
          value: "*.example.com",
          effect: "allow",
          created_at: "2026-08-01T00:00:00Z",
        },
      ],
    };
    renderMember();
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.queryByText(DENIED.APPROVE_CHIP)).not.toBeInTheDocument();
  });

  it("a stale group snapshot is named — but only once something is being enforced", async () => {
    mockCaps = { ...mockCaps, groups_snapshot_stale: true };
    const { unmount } = renderMember();
    await screen.findByRole("button", { name: /^approve$/i });
    expect(screen.queryByText(DENIED.STALE_GROUPS)).not.toBeInTheDocument();
    unmount();

    mockCaps = { ...mockCaps, enforcement: { egress_host: true }, groups_snapshot_stale: true };
    renderMember();
    expect(await screen.findByText(DENIED.STALE_GROUPS)).toBeInTheDocument();
  });
});
