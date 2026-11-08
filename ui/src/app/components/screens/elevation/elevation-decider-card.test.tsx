/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The elevation decider card under Waiting on you. Expected strings are the approved copy's, spelled out.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GovernanceRequest } from "../../../lib/types/governance-requests";

const approveMock = vi.fn();
const rejectMock = vi.fn();
vi.mock("../../../lib/api/governance-requests", () => ({
  requests: { approve: (...a: unknown[]) => approveMock(...a), reject: (...a: unknown[]) => rejectMock(...a) },
}));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { ElevationDeciderCard } from "./elevation-decider-card";

function request(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return {
    id: "c1",
    target_kind: "governance_elevation",
    op: "create",
    target_key: "u1",
    state: "pending",
    kind: "elevation",
    unit_id: "u1",
    unit_name: "Payments",
    duration: "4h",
    proposed_by: "dana.reyes@acme.example",
    proposed_at: aheadByHours(-0.05),
    expires_at: aheadByHours(24),
    diff: { before: null, after: null, changed: [] },
    note: { reason: "Production incident: I need to deploy the hotfix.", ticket: "INC-2210" },
    mine: false,
    decidable: true,
    deciders: [{ kind: "unit_admin" }, { kind: "group", name: "support-leads" }, { kind: "security" }],
    ...over,
  } as GovernanceRequest;
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const setup = (r: GovernanceRequest = request()) => {
  const onDone = vi.fn();
  render(<ElevationDeciderCard request={r} onDone={onDone} />);
  return { onDone };
};

beforeEach(() => {
  approveMock.mockReset();
  rejectMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
});

describe("ElevationDeciderCard: what it says", () => {
  it("names the kind, the person, the unit and the length", () => {
    setup();
    expect(screen.getByText("Elevation")).toBeInTheDocument();
    expect(screen.getByText("Elevate dana.reyes@acme.example to Payments for 4 hours")).toBeInTheDocument();
    expect(screen.getByText(/^Requested by/)).toHaveTextContent("Requested by dana.reyes@acme.example");
  });

  it("shows their reason, and the ticket in mono on the Requested by row", () => {
    setup();
    expect(screen.getByText("Their reason:").parentElement).toHaveTextContent("Their reason: Production incident: I need to deploy the hotfix.");
    expect(screen.getByText("Their reason:").parentElement).not.toHaveTextContent("INC-2210");
    const ticket = screen.getByText("INC-2210");
    expect(ticket).toHaveClass("font-mono");
    expect(ticket.closest("div")).toHaveTextContent("Requested by dana.reyes@acme.example Ticket INC-2210");
  });

  it("shows no ticket when there is none", () => {
    setup(request({ note: { reason: "Need it." } }));
    expect(screen.queryByText("INC-2210")).not.toBeInTheDocument();
    expect(screen.queryByText("Ticket")).not.toBeInTheDocument();
    expect(screen.getByText("Their reason:").parentElement).toHaveTextContent("Their reason: Need it.");
  });

  it("says when it was requested", () => {
    setup();
    expect(screen.getByText("requested 3m ago")).toBeInTheDocument();
  });

  it("states what approving does and its blast radius", () => {
    setup();
    expect(screen.getByText("What you're approving:").parentElement).toHaveTextContent(
      "dana.reyes@acme.example becomes a member of Payments for 4 hours, starting when you approve.",
    );
    expect(screen.getByText("Blast radius:").parentElement).toHaveTextContent(
      "limits until then, and each run they start ends when the elevation ends. Ending the elevation early doesn't stop a run that has already started.",
    );
  });

  it("says who decides, and reads a unit admin with no name as admins of this unit", () => {
    setup();
    expect(screen.getByText("Decided by:").parentElement).toHaveTextContent(
      "Decided by: admins of this unit, the support-leads group or a security admin",
    );
  });
});

describe("ElevationDeciderCard: deciding", () => {
  it("Approve calls approve with the id, says so and finishes", async () => {
    const approve = deferred<unknown>();
    approveMock.mockReturnValue(approve.promise);
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: /Approve/ }));
    expect(approveMock).toHaveBeenCalledWith("c1");
    expect(onDone).not.toHaveBeenCalled();
    approve.resolve({});
    await vi.waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Request approved");
  });

  it("Deny calls reject with the id and no reason", async () => {
    rejectMock.mockResolvedValue({});
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: /Deny/ }));
    expect(rejectMock).toHaveBeenCalledWith("c1", "");
    await vi.waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Request denied");
  });

  it("a failed approve is a toast with the server's sentence", async () => {
    approveMock.mockRejectedValue(new HttpError(409, "That request was already decided.", "governance_change_not_pending"));
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: /Approve/ }));
    await vi.waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Couldn't approve this request", { description: "That request was already decided." });
    expect(onDone).not.toHaveBeenCalled();
  });

  it("a failed deny is a toast with the server's sentence", async () => {
    rejectMock.mockRejectedValue(new HttpError(403, "You may not decide this request.", "not_a_decider"));
    setup();
    await userEvent.click(screen.getByRole("button", { name: /Deny/ }));
    await vi.waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Couldn't deny this request", { description: "You may not decide this request." });
  });
});
