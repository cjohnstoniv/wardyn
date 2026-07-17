/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";

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

// Hand-rolled api fake. listApprovals returns one pending credential request;
// deny() rejects to simulate a 409 / network failure.
const denyMock = vi.fn();
const approveMock = vi.fn();
vi.mock("../../lib/api/approvals", () => {
  const pending: ApprovalRequest = {
    id: "apr_1",
    run_id: "run_1",
    kind: "credential",
    requested_scope: { host: "api.example.com" },
    state: "PENDING",
    requested_at: new Date().toISOString(),
  };
  return {
    approvals: {
      listApprovals: (state: string) =>
        Promise.resolve(state === "PENDING" ? [pending] : []),
      deny: (...a: unknown[]) => denyMock(...a),
      approve: (...a: unknown[]) => approveMock(...a),
    },
  };
});
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

describe("ApprovalsScreen — deny error handling", () => {
  beforeEach(() => {
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
