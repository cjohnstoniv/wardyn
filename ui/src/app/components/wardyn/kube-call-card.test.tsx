/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApprovalRequest, KubeCallScope } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { DECIDERS_INBOX } from "./copy/deciders-inbox";
import { APPROVAL_MCP_CALL } from "./copy/approvals";
import { KUBE_CALL_CARD as C } from "./copy/files-review";
import { KubeCallCard } from "./kube-call-card";

const KEY = "5be1c09f7a2d".padEnd(64, "0");
const BODY_TEXT = '{\n  "spec": {\n    "replicas": 9007199254740993\n  }\n}';

const patch: KubeCallScope = {
  cluster: "k8s.staging.acme.example",
  namespace: "payments-staging",
  method: "PATCH",
  path: "/apis/apps/v1/namespaces/payments-staging/deployments/checkout",
  query: "fieldManager=kubectl-rollout",
  media_type: "application/strategic-merge-patch+json",
  body: { spec: { replicas: 9007199254740992 } },
  call_sha256: KEY,
  hold_seconds: 90,
};
const del: KubeCallScope = {
  cluster: "k8s.staging.acme.example",
  namespace: "payments-staging",
  method: "DELETE",
  path: "/api/v1/namespaces/payments-staging/pods/checkout-7f9c5d8b6-x2lqp",
  call_sha256: "0c7d44e19b3a".padEnd(64, "0"),
  hold_seconds: 90,
};

const row = (s: KubeCallScope, over: Partial<ApprovalRequest> = {}) =>
  ({
    id: "apr_kube",
    run_id: "run_1",
    kind: "tool_call",
    state: "PENDING",
    requested_at: new Date(Date.now() - 40_000).toISOString(),
    requested_scope: s,
    held: true,
    ...over,
  }) as ApprovalRequest & { requested_scope: KubeCallScope };

function renderCard(item: ReturnType<typeof row>, canDecide = true) {
  const onApprove = vi.fn();
  const onDeny = vi.fn();
  render(
    <OperatorProvider operator={false} securityOperator={false} principal="dana@acme.example">
      <KubeCallCard item={item} canDecide={canDecide} run={null} busy={null} onApprove={onApprove} onDeny={onDeny} />
    </OperatorProvider>,
  );
  return { onApprove, onDeny };
}

describe("the kubectl change card", () => {
  it("shows the exact request, its body text, the fingerprint and both notes (frame D)", async () => {
    const { onApprove } = renderCard(row(patch, { call_text: { arguments: BODY_TEXT }, deciders: ["owner", "security"] }));
    const card = screen.getByTestId("kube-call-card");
    expect(within(card).getByText(C.TITLE)).toBeInTheDocument();
    expect(within(card).getByText(APPROVAL_MCP_CALL.ONCE)).toBeInTheDocument();
    expect(card).toHaveTextContent(`${DECIDERS_INBOX.DECIDED_BY_LABEL} you or a security admin`);
    expect(within(card).getAllByRole("term").map((d) => d.textContent)).toEqual([
      C.FIELD_CLUSTER, C.FIELD_NAMESPACE, C.FIELD_METHOD, C.FIELD_PATH, C.FIELD_QUERY, C.FIELD_MEDIA,
    ]);
    expect(within(card).getAllByRole("definition").map((d) => d.textContent)).toEqual([
      patch.cluster, patch.namespace, "PATCH", patch.path, patch.query, patch.media_type,
    ]);
    expect(card).toHaveTextContent(C.WHAT("PATCH", patch.cluster, patch.namespace));
    expect(card).toHaveTextContent(C.BLAST);
    // The server's text, digit for digit; the scope's parsed body would read ...992.
    expect(within(card).getByRole("region", { name: C.FIELD_BODY })).toHaveTextContent("9007199254740993");
    expect(card).not.toHaveTextContent("9007199254740992");
    expect(card).toHaveTextContent(APPROVAL_MCP_CALL.FINGERPRINT("5be1c09f7a2d"));
    expect(card).toHaveTextContent(C.HELD_NOTE);
    expect(card).toHaveTextContent(APPROVAL_MCP_CALL.DENY_NOTE);
    const approve = within(card).getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE });
    expect(approve).toBeEnabled();
    await userEvent.click(approve);
    expect(onApprove).toHaveBeenCalledTimes(1);
  });

  it("with no call_text draws the fallback, Approve off, Deny on, and the not-waiting line (frame D2)", async () => {
    const { onDeny } = renderCard(row(patch, { held: false }));
    const card = screen.getByTestId("kube-call-card");
    expect(card).toHaveTextContent(C.NO_TEXT);
    expect(within(card).queryByRole("region", { name: C.FIELD_BODY })).toBeNull();
    expect(within(card).getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeDisabled();
    expect(card).toHaveTextContent(C.HELD_EXPIRED);
    await userEvent.click(within(card).getByRole("button", { name: APPROVAL_MCP_CALL.DENY }));
    expect(onDeny).toHaveBeenCalledTimes(1);
  });

  it("a bodiless write with empty text reads No body and Approve stays on (frame D3)", async () => {
    const { onApprove } = renderCard(row(del, { call_text: { arguments: "" } }));
    const card = screen.getByTestId("kube-call-card");
    expect(within(card).getByText(C.NO_BODY)).toBeInTheDocument();
    expect(card).not.toHaveTextContent(C.NO_TEXT);
    const approve = within(card).getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE });
    expect(approve).toBeEnabled();
    await userEvent.click(approve);
    expect(onApprove).toHaveBeenCalledTimes(1);
  });

  it("hides Query and Content type when the request sent none", () => {
    renderCard(row(del, { call_text: { arguments: "" } }));
    const card = screen.getByTestId("kube-call-card");
    expect(within(card).queryByText(C.FIELD_QUERY)).toBeNull();
    expect(within(card).queryByText(C.FIELD_MEDIA)).toBeNull();
    expect(within(card).getAllByRole("term").map((d) => d.textContent)).toEqual([C.FIELD_CLUSTER, C.FIELD_NAMESPACE, C.FIELD_METHOD, C.FIELD_PATH]);
  });

  it("a viewer who may not decide gets no buttons", () => {
    renderCard(row(patch, { call_text: { arguments: BODY_TEXT } }), false);
    expect(screen.queryByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeNull();
  });
});
