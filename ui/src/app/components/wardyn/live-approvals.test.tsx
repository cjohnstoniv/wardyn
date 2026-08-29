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

  // W19-S1-3 / W20-hold-fsm-2: the proxy's real hold times out at 30s
  // (defaultHoldTimeout) while the approval row stays PENDING for up to 24h —
  // isHeld must stop claiming a live hold once that window has passed.
  it("stops flagging a wait_for_review request as held once the proxy's 30s hold has elapsed", async () => {
    const stale = new Date(Date.now() - 31_000).toISOString();
    listApprovalsMock.mockResolvedValue([
      pending({ id: "stale", requested_scope: { host: "stale.example", mode: "wait_for_review" }, requested_at: stale }),
    ]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText("stale.example")).toBeInTheDocument();
    expect(within(panel).queryByText("waiting")).not.toBeInTheDocument();
    expect(within(panel).queryByText(/Sandbox is waiting/i)).not.toBeInTheDocument();
  });

  // W20-hold-fsm-5: a poll failure must not render the same affirmative
  // "Watching for…" text a confirmed-empty poll gets.
  it("shows a poll-error state instead of the idle hint when listApprovals rejects with nothing pending", async () => {
    listApprovalsMock.mockRejectedValue(new Error("network error"));
    render(<LiveApprovals runId="r1" />);
    expect(await screen.findByTestId("live-approvals-poll-error")).toBeInTheDocument();
    expect(screen.queryByTestId("live-approvals-idle")).not.toBeInTheDocument();
  });

  // {host,secret_name} with no header/format classifies as git_pat under the
  // credentialKind heuristic (copy.ts) — a resident, agent-readable PAT whose
  // blast-radius card (TTL, "the agent's process can read this") is the whole
  // point of deciding it, so it stays off this strip and routes via the
  // Approvals screen's kind-aware card instead.
  it("a pending git_pat-shaped credential stays out of the strip (its blast-radius card lives on the Approvals screen)", async () => {
    listApprovalsMock.mockResolvedValue([
      pending({ id: "cred", kind: "credential", requested_scope: { host: "api.example", secret_name: "x" } }),
    ]);
    render(<LiveApprovals runId="r1" />);
    expect(await screen.findByTestId("live-approvals-idle")).toBeInTheDocument();
    expect(screen.queryByTestId("live-approvals")).not.toBeInTheDocument();
  });

  // An api_key-shaped scope (carries `header`) is the opposite case: the
  // secrets demos' authorized-not-issued mint is raised from INSIDE the
  // sandbox mid-run, over the broker's own route — the same
  // decision-visible-where-it-happens principle egress/tool_call rows already
  // get, so it renders on the strip, labeled by host like an egress row.
  it("a pending api_key-shaped credential renders on the strip, labeled by host", async () => {
    listApprovalsMock.mockResolvedValue([
      pending({
        id: "cred-key",
        kind: "credential",
        requested_scope: { host: "example.com", header: "X-Wardyn-Demo", secret_name: "wardyn-demo-key" },
      }),
    ]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText("example.com")).toBeInTheDocument();
  });

  // Deny's confirm copy must own up to what the click actually refuses: a
  // mint, not the host. The egress sentence ("blocks this host…") would be
  // FALSE here — denying withholds the credential; the allowlist is
  // unaffected. Same pin shape as the tool_call precedent above.
  it("denying an api_key-shaped credential shows the mint-refusal copy, not the egress host sentence", async () => {
    listApprovalsMock.mockResolvedValue([
      pending({
        id: "cred-key",
        kind: "credential",
        requested_scope: { host: "example.com", header: "X-Wardyn-Demo", secret_name: "wardyn-demo-key" },
      }),
    ]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(within(panel).getByRole("button", { name: /^deny$/i }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/refuses this credential mint/i)).toBeInTheDocument();
    expect(within(dialog).queryByText(/blocks this host/i)).not.toBeInTheDocument();
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

    // The toolgate blocks the agent ON the PENDING row, so a tool-only strip is
    // a HELD strip (isHeld) and takes the held heading — never the egress one.
    it("never claims off-policy egress over a tool-only strip, and says the sandbox is waiting", async () => {
      listApprovalsMock.mockResolvedValue([toolRow()]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      expect(within(panel).queryByText(/off-policy egress/i)).not.toBeInTheDocument();
      expect(within(panel).getByText(/Sandbox is waiting/i)).toBeInTheDocument();
      // …and the row flags the live hold, like a wait_for_review egress row.
      expect(within(panel).getByText("waiting")).toBeInTheDocument();
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

  // D7: a pending row whose host is the agent CLI's known telemetry endpoint
  // gets an identification tag; an ordinary off-policy host does not.
  describe("D7 — known-telemetry tag", () => {
    it("tags a row matching the known telemetry host, and only that row", async () => {
      listApprovalsMock.mockResolvedValue([
        pending({ id: "telemetry", requested_scope: { host: "http-intake.logs.us5.datadoghq.com" } }),
        pending({ id: "other", requested_scope: { host: "api.internal.acme.com" } }),
      ]);
      render(<LiveApprovals runId="r1" />);
      const panel = await screen.findByTestId("live-approvals");
      const rows = within(panel).getAllByTestId("live-approval-row");
      expect(within(rows[0]).getByText("Agent telemetry")).toBeInTheDocument();
      expect(within(rows[1]).queryByText("Agent telemetry")).not.toBeInTheDocument();
    });
  });
});

// ---------------------------------------------------------------------------
// M3 — the strip sits UNDER the terminal it belongs to, so it caps at two rows
// and offers the rest behind the board's own "Show all N". That cap is what
// makes the ordering load-bearing: an implicit "whatever the API returned"
// order was harmless while every row was visible, and hides the one decision
// parking the sandbox the moment it is not.
// ---------------------------------------------------------------------------
describe("LiveApprovals — precedence and the two-row overflow", () => {
  beforeEach(() => {
    listApprovalsMock.mockReset().mockResolvedValue([]);
    approveMock.mockReset().mockResolvedValue({});
    denyMock.mockReset().mockResolvedValue({});
  });

  /** n passive rows, then one held row LAST — the order the API might send. */
  function heldLast(passiveCount: number): ApprovalRequest[] {
    const rows = Array.from({ length: passiveCount }, (_, i) =>
      pending({ id: `p${i}`, requested_scope: { host: `passive${i}.example` } }),
    );
    return [
      ...rows,
      pending({
        id: "held",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "held.example", mode: "wait_for_review" },
      }),
    ];
  }

  it("shows at most two rows and offers the rest — the strip never grows past the terminal", async () => {
    listApprovalsMock.mockResolvedValue(heldLast(3));
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getAllByTestId("live-approval-row")).toHaveLength(2);
    expect(within(panel).getByTestId("live-approvals-show-all")).toHaveTextContent("Show all 4");
  });

  it("puts the HELD request first, even when the API sends it last — held outranks passive", async () => {
    listApprovalsMock.mockResolvedValue(heldLast(3));
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const rows = within(panel).getAllByTestId("live-approval-row");
    expect(rows[0]).toHaveTextContent("held.example");
    // The regression the cap creates: the one decision parking the sandbox
    // must never be the row behind "Show all".
    expect(within(panel).getByText("held.example")).toBeInTheDocument();
  });

  it("Show all reveals every row, and folds back", async () => {
    listApprovalsMock.mockResolvedValue(heldLast(3));
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");

    await userEvent.click(within(panel).getByTestId("live-approvals-show-all"));
    expect(within(panel).getAllByTestId("live-approval-row")).toHaveLength(4);
    expect(within(panel).getByTestId("live-approvals-show-all")).toHaveTextContent("Show fewer");

    await userEvent.click(within(panel).getByTestId("live-approvals-show-all"));
    expect(within(panel).getAllByTestId("live-approval-row")).toHaveLength(2);
  });

  it("offers no overflow control when everything already fits", async () => {
    listApprovalsMock.mockResolvedValue(heldLast(1));
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getAllByTestId("live-approval-row")).toHaveLength(2);
    expect(within(panel).queryByTestId("live-approvals-show-all")).not.toBeInTheDocument();
  });

  it("disables the row the instant Approve fires, without flashing a spinner (CONSOLE-RULES §7)", async () => {
    listApprovalsMock.mockResolvedValue([pending()]);
    let release: (v: unknown) => void = () => {};
    approveMock.mockImplementation(() => new Promise((r) => (release = r)));
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const approve = within(panel).getByRole("button", { name: /^Approve$/ });

    await userEvent.click(approve);
    // Disabled immediately; useDeferredBusy holds the spinner back, so a fast
    // response never flashes one.
    expect(approve).toBeDisabled();
    expect(panel.querySelector(".animate-spin")).toBeNull();
    release({});
  });
});
