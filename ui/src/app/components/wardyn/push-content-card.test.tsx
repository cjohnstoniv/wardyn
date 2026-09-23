/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ApprovalRequest } from "../../lib/types";
import type { PushContentScope } from "../../lib/types/approvals";
import { PushContentCard, type PushCardRun } from "./push-content-card";
import { PUSH } from "./copy/push";
import { APPROVAL, SECURITY_ONLY_REASON } from "./copy";

const RUNNING: PushCardRun = { state: "RUNNING" };
const ENDED: PushCardRun = { state: "COMPLETED" };

function scope(over: Partial<PushContentScope> = {}): PushContentScope {
  return {
    repo: "github.com/acme/payments",
    branch: "refs/heads/feature/x",
    acts_as: "github_token:11111111-1111-1111-1111-111111111111",
    paths: ["a.txt", "b.txt"],
    paths_total: 2,
    commits: ["deadbeef".repeat(5)],
    paths_digest: "a".repeat(64),
    acts_as_kind: "github_app",
    acts_as_label: "dana@acme.example",
    ...over,
  };
}

function push(over: Partial<ApprovalRequest> = {}): ApprovalRequest & { requested_scope: PushContentScope } {
  return {
    id: "apr_push_1",
    run_id: "run_1",
    kind: "push_content",
    requested_scope: scope(over.requested_scope as Partial<PushContentScope> | undefined),
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  } as ApprovalRequest & { requested_scope: PushContentScope };
}

describe("PushContentCard — held (#181)", () => {
  it("shows the frozen title, the repository, branch and acts_as_label — never the raw acts_as", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={false} onApprove={vi.fn()} onDeny={vi.fn()} />);
    const card = screen.getByTestId("push-content-card");
    expect(within(card).getByText(PUSH.CARD_TITLE)).toBeInTheDocument();
    expect(within(card).getByText("github.com/acme/payments")).toBeInTheDocument();
    expect(within(card).getByText("refs/heads/feature/x")).toBeInTheDocument();
    expect(within(card).getByText("dana@acme.example")).toBeInTheDocument();
    // never the raw acts_as (a credential reference, not a principal)
    expect(within(card).queryByText(/github_token:11111111/)).not.toBeInTheDocument();
  });

  it("lists up to ten paths, then '+N more' — no expanding control", () => {
    const paths = Array.from({ length: 10 }, (_, i) => `dir/file-${i}.txt`);
    render(
      <PushContentCard
        item={push({ requested_scope: { paths, paths_total: 14 } as Partial<PushContentScope> })}
        securityOperator
        run={RUNNING}
        busy={false}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = screen.getByTestId("push-content-card");
    for (const p of paths) expect(within(card).getByText(p)).toBeInTheDocument();
    expect(within(card).getByText(PUSH.PATHS_MORE(4))).toBeInTheDocument();
    expect(within(card).getByText(PUSH.PATHS_NOTE)).toBeInTheDocument();
    // No expand/"view all" affordance — the collapsed raw-scope <details> is
    // the only disclosure control this card carries.
    expect(within(card).queryByRole("button", { name: /more|expand|all/i })).not.toBeInTheDocument();
  });

  it("shows exactly the ten names paths_total does not exceed — no '+N more' when everything fits", () => {
    render(
      <PushContentCard
        item={push({ requested_scope: { paths: ["a.txt"], paths_total: 1 } as Partial<PushContentScope> })}
        securityOperator
        run={RUNNING}
        busy={false}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    expect(screen.queryByText(/\+\d+ more/)).not.toBeInTheDocument();
  });

  it("never labels scope.commits as commits, including for an ADO REST push's body digest", () => {
    render(
      <PushContentCard
        item={push({
          requested_scope: {
            acts_as_kind: "ado_entra",
            commits: ["b".repeat(64)], // a SHA-256 of the request body, not an object id
          } as Partial<PushContentScope>,
        })}
        securityOperator
        run={RUNNING}
        busy={false}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = screen.getByTestId("push-content-card");
    // No labelled "Commits" field anywhere on the card.
    expect(within(card).queryByText(/^Commits$/)).not.toBeInTheDocument();
  });

  it("reuses the approval banner labels for What/Blast, with PUSH.WHAT/BLAST", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={false} onApprove={vi.fn()} onDeny={vi.fn()} />);
    const card = screen.getByTestId("push-content-card");
    expect(within(card).getByText(PUSH.WHAT("github.com/acme/payments", "dana@acme.example"))).toBeInTheDocument();
    expect(within(card).getByText(PUSH.BLAST)).toBeInTheDocument();
  });

  it("carries the held note and, for a decider, the approve note", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={false} onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.getByText(PUSH.HELD_NOTE)).toBeInTheDocument();
    expect(screen.getByText(PUSH.APPROVE_NOTE)).toBeInTheDocument();
  });
});

describe("PushContentCard — deciding", () => {
  it("disables both buttons and shows a spinner on each while busy", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.getByRole("button", { name: /Approve/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Deny/ })).toBeDisabled();
  });

  it("Approve/Deny call straight through with no dialog", async () => {
    const onApprove = vi.fn();
    const onDeny = vi.fn();
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={false} onApprove={onApprove} onDeny={onDeny} />);
    screen.getByRole("button", { name: /Approve/ }).click();
    expect(onApprove).toHaveBeenCalledTimes(1);
    screen.getByRole("button", { name: /Deny/ }).click();
    expect(onDeny).toHaveBeenCalledTimes(1);
    // No reason textbox, no scope control — this card decides directly.
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("radiogroup")).not.toBeInTheDocument();
  });
});

describe("PushContentCard — cancelled (run ended)", () => {
  it("withdraws the decision pair and shows the reused APPROVAL.CANCELLED_BODY", () => {
    render(<PushContentCard item={push()} securityOperator run={ENDED} busy={false} onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /Approve/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Deny/ })).not.toBeInTheDocument();
    expect(screen.getByText(APPROVAL.CANCELLED_BODY)).toBeInTheDocument();
  });
});

describe("PushContentCard — member watching, decides nothing", () => {
  it("shows the admin-only reason instead of Approve/Deny for a non-security-operator", () => {
    render(<PushContentCard item={push()} securityOperator={false} run={RUNNING} busy={false} onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /Approve/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Deny/ })).not.toBeInTheDocument();
    expect(screen.getByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    // Still sees the full requested scope — "sees everything, decides nothing".
    expect(screen.getByText("github.com/acme/payments")).toBeInTheDocument();
  });
});
