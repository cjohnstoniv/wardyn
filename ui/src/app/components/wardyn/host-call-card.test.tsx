/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApprovalRequest, HostCallScope } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { DECIDERS_INBOX } from "./copy/deciders-inbox";
import { APPROVAL, APPROVAL_MCP_CALL, APPROVAL_SCOPE_LABEL } from "./copy/approvals";
import { HOST_CALL_CARD as C } from "./copy/files-review";
import { HostCallCard } from "./host-call-card";

const scope: HostCallScope = { host: "api.payments.example.com", method: "POST", path: "/v1/refunds", call_sha256: "7ab4c1e09d3f".padEnd(64, "0"), hold_seconds: 90 };

const row = (s: HostCallScope = scope, over: Partial<ApprovalRequest> = {}) =>
  ({
    id: "apr_host",
    run_id: "run_1",
    kind: "tool_call",
    state: "PENDING",
    requested_at: new Date(Date.now() - 40_000).toISOString(),
    requested_scope: s,
    held: true,
    ...over,
  }) as ApprovalRequest & { requested_scope: HostCallScope };

function renderCard(item: ReturnType<typeof row>, canDecide = true, run: { state: string } | null = null) {
  const onApprove = vi.fn();
  const onDeny = vi.fn();
  render(
    <OperatorProvider operator={false} securityOperator={false} principal="dana@acme.example">
      <HostCallCard item={item} canDecide={canDecide} run={run as never} busy={null} onApprove={onApprove} onDeny={onDeny} />
    </OperatorProvider>,
  );
  return { onApprove, onDeny };
}

const inFuture = () => new Date(Date.now() + 60_000).toISOString();
const inPast = () => new Date(Date.now() - 60_000).toISOString();

describe("the host call card (HC-1)", () => {
  it("shows host, method and path, the run-wide sentences and the verdict's exact strings", async () => {
    const { onApprove, onDeny } = renderCard(row(scope, { held_until: inFuture(), deciders: ["unit_admins"] }));
    const card = screen.getByTestId("host-call-card");
    expect(within(card).getByText("Approve requests to one host")).toBeInTheDocument();
    expect(within(card).getByText(APPROVAL_SCOPE_LABEL.run)).toBeInTheDocument();
    expect(within(card).getAllByRole("term").map((d) => d.textContent)).toEqual(["Host", "Method", "Path"]);
    expect(within(card).getAllByRole("definition").map((d) => d.textContent)).toEqual([scope.host, "POST", "/v1/refunds"]);
    expect(card).toHaveTextContent(
      "allowing POST requests to /v1/refunds on api.payments.example.com for the rest of this run.",
    );
    expect(card).toHaveTextContent("This covers any query or body sent with this host, method and path. Other policy rules still apply.");
    expect(card).toHaveTextContent("If the request is still waiting, approving lets it proceed when all other rules allow it.");
    expect(card).toHaveTextContent("Refuses this method and path on this host for the rest of the run.");
    expect(card).not.toHaveTextContent("Another method or path asks again");
    expect(within(card).queryByText(APPROVAL_MCP_CALL.ONCE)).toBeNull();
    expect(card).not.toHaveTextContent("Approve once");
    await userEvent.click(within(card).getByRole("button", { name: "Approve for this run" }));
    expect(onApprove).toHaveBeenCalledTimes(1);
    await userEvent.click(within(card).getByRole("button", { name: "Deny" }));
    expect(onDeny).toHaveBeenCalledTimes(1);
  });

  it("after the hold deadline, reads the no-longer-waiting note", () => {
    renderCard(row(scope, { held: false, held_until: inPast() }));
    const card = screen.getByTestId("host-call-card");
    expect(card).toHaveTextContent(
      "No longer waiting. This approval covers retries with the same host, method and path for the rest of the run. Other policy rules still apply.",
    );
    expect(card).not.toHaveTextContent(C.HELD_NOTE);
  });

  it("renders no held or expired note when the wire supplies no hold deadline", () => {
    renderCard(row(scope, { held: true }));
    const card = screen.getByTestId("host-call-card");
    expect(card).not.toHaveTextContent(C.HELD_NOTE);
    expect(card).not.toHaveTextContent(C.HELD_EXPIRED);
    // hold_seconds (in the scope above) is a duration, not a deadline.
    expect(card).not.toHaveTextContent("No longer waiting");
  });

  it("a long path wraps in its definition rather than clipping", () => {
    const path = "/v1/customers/cus_9f3b27e1a4c8/payment-methods/pm_1Q2w3e4r5t6y7u8i/refunds";
    renderCard(row({ ...scope, path }));
    const card = screen.getByTestId("host-call-card");
    const dd = within(card).getAllByRole("definition")[2];
    expect(dd).toHaveTextContent(path);
    expect(dd.className).toContain("break-all");
  });

  it("a known terminal run withdraws the buttons and says nothing was decided", () => {
    renderCard(row(scope, { held_until: inFuture() }), true, { state: "COMPLETED" });
    const card = screen.getByTestId("host-call-card");
    expect(card).toHaveTextContent(APPROVAL.CANCELLED_BODY);
    expect(within(card).queryByRole("button")).toBeNull();
    expect(card).not.toHaveTextContent(C.HELD_NOTE);
  });

  it("a viewer who may not decide gets no buttons, and the second-person line shows", () => {
    renderCard(row(scope, { requested_by: "dana@acme.example" } as Partial<ApprovalRequest>), false);
    expect(screen.queryByRole("button", { name: C.APPROVE })).toBeNull();
    expect(screen.getByTestId("host-call-card")).toHaveTextContent(APPROVAL_MCP_CALL.CANNOT_DECIDE);
    expect(DECIDERS_INBOX.DECIDED_BY_LABEL).toBeTruthy();
  });
});
