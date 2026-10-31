/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// T4-c (board R16): a request whose host the run owner's own walls deny has Approve off, Deny on, a sentence
// naming whose walls they are, and the Why card under it.
import { beforeEach, describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { makeApproval } from "../../../test/factories";
import { SCENARIO_HARNESSES } from "../../lib/new-run-fixtures/scenarios/harnesses";
import type { RunDetail, SetupStatus } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { OperatorProvider } from "../wardyn/operator-context";
import { ApprovalCard } from "./approvals-cards";

const getRun = vi.hoisted(() => vi.fn());
vi.mock("../../lib/api/runs", () => ({ runs: { getRun } }));
const RUN = { id: "r1", created_by: "ana@example.com", agent: "claude-code", repo: "acme/web-app", title: "Fix the flaky cart test", state: "RUNNING", confinement_class: "CC1" } satisfies Partial<RunDetail>;
beforeEach(() => { getRun.mockResolvedValue(RUN); });

const row = (walls_block?: boolean) =>
  makeApproval({
    id: "a1", run_id: "r1", kind: "egress_domain", state: "PENDING", requested_at: "",
    requested_scope: { host: "paste.example.net" }, walls_block,
  });
const show = (it: ReturnType<typeof row>, principal: string) =>
  render(
    <MemoryRouter>
      <OperatorProvider operator principal={principal}>
        <ModelAccessProvider status={{ harnesses: SCENARIO_HARNESSES.full } as SetupStatus} onRefresh={() => {}}>
          <ApprovalCard item={it} caps={null} onAct={() => {}} onAdoDecide={async () => {}} onPushDecide={async () => {}} />
        </ModelAccessProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );

describe("ApprovalCard: a host the requester's walls deny", () => {
  it("turns Approve off, keeps Deny, and speaks to the owner in the first person", async () => {
    show(row(true), "ana@example.com");
    const note = await screen.findByTestId("approval-walls-block");
    expect(note).toHaveTextContent("Your walls block paste.example.net. Change them in Settings.");
    expect(note).toHaveTextContent("Set byYour walls");
    expect(note).toHaveTextContent("Ownerana@example.com");
    expect(note).toHaveTextContent("Can changeYou can, in Settings.");
    expect(screen.getByRole("button", { name: /approve/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /deny/i })).toBeEnabled();
  });

  it("names the owner's walls to an approver who is not the owner", async () => {
    show(row(true), "lena@example.com");
    const note = await screen.findByTestId("approval-walls-block");
    expect(note).toHaveTextContent("ana@example.com's walls block paste.example.net for this run.");
    expect(note).toHaveTextContent("Can changeNot from here. The owner can.");
    expect(screen.getByRole("button", { name: /approve/i })).toBeDisabled();
  });

  it("draws none of it without the server's fact", async () => {
    show(row(), "ana@example.com");
    await screen.findByText("Fix the flaky cart test");
    expect(screen.queryByTestId("approval-walls-block")).toBeNull();
    expect(screen.getByRole("button", { name: /approve/i })).toBeEnabled();
  });

  it("draws no sentence when the run read fails, since whose walls they are is unknown", async () => {
    getRun.mockRejectedValue(new Error("forbidden"));
    show(row(true), "lena@example.com");
    await screen.findByRole("button", { name: /deny/i });
    await waitFor(() => expect(getRun).toHaveBeenCalled());
    expect(screen.queryByTestId("approval-walls-block")).toBeNull();
    expect(screen.getByRole("button", { name: /approve/i })).toBeDisabled();
  });
});
