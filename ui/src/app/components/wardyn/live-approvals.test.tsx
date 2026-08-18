/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApprovalRequest } from "../../lib/types";

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

function pending(over: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "a1",
    run_id: "r1",
    kind: "egress_domain",
    requested_scope: { host: "unlisted.example" },
    state: "PENDING",
    requested_at: "",
    ...over,
  } as ApprovalRequest;
}

describe("LiveApprovals", () => {
  beforeEach(() => {
    listApprovalsMock.mockReset().mockResolvedValue([]);
    approveMock.mockReset().mockResolvedValue({});
    denyMock.mockReset().mockResolvedValue({});
  });

  it("shows the idle hint when nothing is pending", async () => {
    render(<LiveApprovals runId="r1" />);
    expect(await screen.findByTestId("live-approvals-idle")).toBeInTheDocument();
  });

  it("flags a wait_for_review request as HELD ('waiting') and only this run's approvals", async () => {
    listApprovalsMock.mockResolvedValue([
      pending({ id: "held", requested_scope: { host: "held.example", mode: "wait_for_review" } }),
      pending({ id: "other", run_id: "other-run", requested_scope: { host: "other.example" } }),
    ]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText("held.example")).toBeInTheDocument();
    expect(within(panel).queryByText("other.example")).not.toBeInTheDocument(); // filtered by runId
    expect(within(panel).getByText("waiting")).toBeInTheDocument(); // per-row held badge (exact)
    expect(within(panel).getByText(/Sandbox is waiting/i)).toBeInTheDocument(); // header
  });

  it("a pending credential for this run stays out of the strip (its blast-radius card lives on the Approvals screen)", async () => {
    listApprovalsMock.mockResolvedValue([
      pending({ id: "cred", kind: "credential", requested_scope: { host: "api.example", secret_name: "x" } }),
    ]);
    render(<LiveApprovals runId="r1" />);
    expect(await screen.findByTestId("live-approvals-idle")).toBeInTheDocument();
    expect(screen.queryByTestId("live-approvals")).not.toBeInTheDocument();
  });

  // The toolgate is tool_call's first producer: a `hold` run parks the agent on
  // every mutating tool call, and this strip is where that run is decided.
  describe("tool_call rows", () => {
    const LONG_CMD = "npm install ".repeat(12).trim();

    function toolRow(over: Partial<ApprovalRequest> = {}) {
      return pending({
        id: "t1",
        kind: "tool_call",
        requested_scope: { tool: "Bash", cmd: "rm -rf build" },
        ...over,
      });
    }

    it("renders tool + cmd, clipped for the strip with the full string on the title", async () => {
      listApprovalsMock.mockResolvedValue([toolRow({ requested_scope: { tool: "Bash", cmd: LONG_CMD } })]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      const full = `Bash: ${LONG_CMD}`;
      const line = within(panel).getByTitle(full);
      expect(line.textContent).toBe(full.slice(0, 71) + "…");
      expect(line.textContent!.length).toBeLessThan(full.length);
    });

    // decide rule 4 (approvals.go): ANY explicit decision_scope on a non-egress
    // approval is a 400 — so a tool row must offer no scope to pick, and must
    // decide with the BODYLESS default (a literal 2-argument api call).
    it("renders NO scope caret and approves with a bodyless decision", async () => {
      listApprovalsMock.mockResolvedValue([toolRow()]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      expect(within(panel).queryAllByRole("button", { name: /more options/i })).toHaveLength(0);

      const user = userEvent.setup({ pointerEventsCheck: 0 });
      await user.click(within(panel).getByRole("button", { name: /approve/i }));
      expect(approveMock).toHaveBeenCalledWith("t1", expect.any(String));
    });

    it("keeps the caret on an egress row in the same strip", async () => {
      listApprovalsMock.mockResolvedValue([toolRow(), pending({ id: "e1", requested_scope: { host: "unlisted.example" } })]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      // Two carets total: approve + deny, from the egress row alone.
      expect(within(panel).getAllByRole("button", { name: /more options/i })).toHaveLength(2);
    });

    it("denies through the confirm dialog, with tool copy — not the egress host sentence", async () => {
      listApprovalsMock.mockResolvedValue([toolRow()]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      const user = userEvent.setup({ pointerEventsCheck: 0 });

      await user.click(within(panel).getByRole("button", { name: /^deny$/i }));
      expect(denyMock).not.toHaveBeenCalled();
      const dialog = await screen.findByRole("alertdialog");
      expect(within(dialog).getByText(/refuses this tool call/i)).toBeInTheDocument();
      expect(within(dialog).queryByText(/blocks this host/i)).not.toBeInTheDocument();

      await user.click(within(dialog).getByRole("button", { name: /^deny$/i }));
      expect(denyMock).toHaveBeenCalledWith("t1", expect.any(String));
    });

    it("never claims off-policy egress over a tool-only strip", async () => {
      listApprovalsMock.mockResolvedValue([toolRow()]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      expect(within(panel).queryByText(/off-policy egress/i)).not.toBeInTheDocument();
      expect(within(panel).getByText(/the agent is waiting on you/i)).toBeInTheDocument();
    });
  });

  it("approves inline via the API", async () => {
    listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example", mode: "wait_for_review" } })]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(panel).getByRole("button", { name: /approve/i }));
    expect(approveMock).toHaveBeenCalledWith("held", expect.any(String));
  });

  // The FULL sentence, leading "Denying" included — a partial match on just
  // "for the rest of this run" would still pass if the trailing clause
  // silently regressed to a claim a different scope falsifies (see
  // copy.ts's denyDialogCopy: `once` re-raises, `always` outlives the run and
  // is undoable, so neither could honestly carry the OLD "no undo and no
  // re-raise" clause this replaced).
  const DENY_RUN_SCOPE_SENTENCE =
    /denying blocks this host for the rest of this run\. the agent won't be able to reach it, and you won't be asked again\./i;

  // W20-W20-hold-fsm-6: a Deny click on the live strip must not go straight to
  // the API — a misclick must be recoverable via Cancel, not just fast.
  it("Deny opens a confirm dialog and does NOT call the API until confirmed", async () => {
    listApprovalsMock.mockResolvedValue([pending({ id: "a1", requested_scope: { host: "risky.example" } })]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(within(panel).getByRole("button", { name: /^deny$/i }));
    expect(denyMock).not.toHaveBeenCalled();
    expect(await screen.findByText(DENY_RUN_SCOPE_SENTENCE)).toBeInTheDocument();

    // Cancel backs out with no API call and no lingering dialog.
    await user.click(screen.getByRole("button", { name: /cancel/i }));
    expect(denyMock).not.toHaveBeenCalled();
    expect(screen.queryByText(DENY_RUN_SCOPE_SENTENCE)).not.toBeInTheDocument();
  });

  it("confirming the Deny dialog calls the API exactly once", async () => {
    listApprovalsMock.mockResolvedValue([pending({ id: "a1", requested_scope: { host: "risky.example" } })]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(within(panel).getByRole("button", { name: /^deny$/i }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: /^deny$/i }));

    expect(denyMock).toHaveBeenCalledTimes(1);
    expect(denyMock).toHaveBeenCalledWith("a1", expect.any(String));
  });

  // Split button, caret side (W20-hold-fsm/egress-scopes): bare click stays
  // "This run" (pinned above); the caret is the other three scopes.
  describe("scope caret", () => {
    it("offers all four scopes, Always disabled (and why) without a workspace", async () => {
      listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example" } })]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      const user = userEvent.setup({ pointerEventsCheck: 0 });

      // Two carets render (approve + deny) — open the first (approve's).
      await user.click(within(panel).getAllByRole("button", { name: /more options/i })[0]);

      expect(await screen.findByText("Once")).toBeInTheDocument();
      expect(screen.getByText("This run")).toBeInTheDocument();
      expect(screen.getByText("Until…")).toBeInTheDocument();
      const always = screen.getByRole("button", { name: /^Always/ });
      expect(always).toBeDisabled(); // real disabled attribute, not aria-disabled
      expect(screen.getByText(/needs a workspace/i)).toBeInTheDocument();
    });

    it("picking Once from the approve caret decides immediately with scope once", async () => {
      listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example" } })]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      const user = userEvent.setup({ pointerEventsCheck: 0 });

      await user.click(within(panel).getAllByRole("button", { name: /more options/i })[0]);
      await user.click(await screen.findByText("Once"));

      expect(approveMock).toHaveBeenCalledWith("held", expect.any(String), { scope: "once", until: undefined });
    });

    it("Always is enabled with hasWorkspace, and picking it decides with scope always", async () => {
      listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example" } })]);
      render(<LiveApprovals runId="r1" hasWorkspace />);
      const panel = await screen.findByTestId("live-approvals");
      const user = userEvent.setup({ pointerEventsCheck: 0 });

      await user.click(within(panel).getAllByRole("button", { name: /more options/i })[0]);
      const always = await screen.findByRole("button", { name: /^Always/ });
      expect(always).not.toBeDisabled();
      await user.click(always);

      expect(approveMock).toHaveBeenCalledWith("held", expect.any(String), { scope: "always", until: undefined });
    });

    it("picking a scope from the deny caret opens the confirm dialog with that scope's copy, not an immediate deny", async () => {
      listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example" } })]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      const user = userEvent.setup({ pointerEventsCheck: 0 });

      // Second caret is the deny one.
      await user.click(within(panel).getAllByRole("button", { name: /more options/i })[1]);
      await user.click(await screen.findByText("Deny once"));

      expect(denyMock).not.toHaveBeenCalled();
      expect(
        await screen.findByText(/denying blocks this one connection\. the agent can try again/i),
      ).toBeInTheDocument();

      const dialog = screen.getByRole("alertdialog");
      await user.click(within(dialog).getByRole("button", { name: /^deny$/i }));
      expect(denyMock).toHaveBeenCalledWith("held", expect.any(String), { scope: "once", until: undefined });
    });

    // The bug: the datetime-local's onChange used to call pick() directly,
    // which for Approve decides on the spot — committing on the FIRST
    // complete value while the operator might still be scrubbing hour/minute.
    // The dialog variant (reason-dialog.tsx) only ever sets state; this one
    // must too, behind the same kind of explicit confirm.
    it("the custom time input does not commit on change — only 'Use this time' does", async () => {
      listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example" } })]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      const user = userEvent.setup({ pointerEventsCheck: 0 });

      await user.click(within(panel).getAllByRole("button", { name: /more options/i })[0]);
      await user.click(await screen.findByText("Until…"));

      // No time chosen yet: the confirm control is disabled, same shape as
      // ReasonDialog's Approve button while untilMissing.
      const confirm = screen.getByRole("button", { name: /use this time/i });
      expect(confirm).toBeDisabled();

      const input = screen.getByLabelText("Pick a time");
      fireEvent.change(input, { target: { value: "2030-01-01T17:00" } });

      // Scrubbing to a complete value must not have decided anything yet.
      expect(approveMock).not.toHaveBeenCalled();
      expect(confirm).not.toBeDisabled();

      await user.click(confirm);
      expect(approveMock).toHaveBeenCalledWith("held", expect.any(String), {
        scope: "until",
        until: new Date("2030-01-01T17:00").toISOString(),
      });
    });
  });
});
