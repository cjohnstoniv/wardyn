/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One MCP call's approval card on the Approvals screen, from the API's wire text to the card. The wire below is
// what the server sends for a call whose numbers a JavaScript Number cannot hold: adjacent integers past 2^53 and
// a decimal with more digits than a float64 keeps. The test reads it with JSON.parse, as the app's client does, so
// requested_scope.arguments arrives rounded; the card must show and copy call_text, the exact text.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { OperatorProvider } from "../wardyn/operator-context";
import { APPROVAL_MCP_CALL as C } from "../wardyn/copy/approvals";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

let mockRows: ApprovalRequest[] = [];
const approveMock = vi.fn();
const denyMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: async (state: string) => mockRows.filter((r) => r.state === state),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: (...a: unknown[]) => denyMock(...a),
  },
}));
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getMyCapabilities: () =>
      Promise.resolve({ grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 } satisfies MeCapabilities),
  },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: () =>
      Promise.resolve({ id: "run_1", agent: "claude-code", repo: "acme/api", task: "t", confinement_class: "CC2", state: "RUNNING", created_by: "dana@acme.example" }),
  },
}));

import { ApprovalsScreen } from "./approvals";

const ARGS_TEXT = '{\n  "ids": [\n    9007199254740993,\n    9007199254740995\n  ],\n  "price": 1.0000000000000001\n}';
const EXTRA_TEXT = '{\n  "task": {\n    "ttl": 18446744073709551617\n  }\n}';
const KEY = "0123456789abcdef".repeat(4);

// The wire text of one pending MCP call row, as GET /approvals sends it.
const wire = (over = "") => `{
  "id": "apr_mcp", "run_id": "run_1", "kind": "tool_call", "state": "PENDING", "requested_at": "2026-11-07T14:02:11Z",
  "requested_scope": {"tool": "mcp__jira__delete_issue", "mcp_server": "jira", "mcp_host": "jira.example.com",
    "arguments": {"ids": [9007199254740993, 9007199254740995], "price": 1.0000000000000001},
    "call_sha256": "${KEY}"},
  "call_text": {"arguments": ${JSON.stringify(ARGS_TEXT)}, "extra": ${JSON.stringify(EXTRA_TEXT)}}${over}
}`;
const row = (over = ""): ApprovalRequest => JSON.parse(wire(over)) as ApprovalRequest;

function renderScreen(operator: boolean, securityOperator: boolean, principal: string, entry = "/approvals") {
  return render(
    <OperatorProvider operator={operator} securityOperator={securityOperator} principal={principal}>
      <MemoryRouter initialEntries={[entry]}>
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <ApprovalsScreen />
        </ModelAccessProvider>
      </MemoryRouter>
    </OperatorProvider>,
  );
}

beforeEach(() => {
  approveMock.mockReset();
  denyMock.mockReset();
  mockRows = [];
});

describe("the MCP call card, from the wire", () => {
  it("shows and copies every digit of the arguments and the other parameters", async () => {
    const parsed = row();
    // What JSON.parse made of the scope: the digits this card must never render.
    expect(JSON.stringify(parsed.requested_scope.arguments)).toContain("9007199254740992");
    mockRows = [parsed];
    renderScreen(false, true, "admin@acme.example");
    const card = await screen.findByTestId("mcp-call-card");

    const args = within(card).getByRole("region", { name: C.ARGUMENTS });
    expect(args.textContent).toBe(ARGS_TEXT);
    expect(within(card).getByRole("region", { name: C.EXTRA }).textContent).toBe(EXTRA_TEXT);
    expect(card.textContent).not.toContain("9007199254740992");

    // userEvent's own clipboard stands in for the browser's: what the button wrote is what a paste would read.
    const user = userEvent.setup();
    await user.click(within(card).getByRole("button", { name: C.COPY_ARGUMENTS }));
    expect(await navigator.clipboard.readText()).toBe(ARGS_TEXT);
    await user.click(within(card).getByRole("button", { name: C.COPY_EXTRA }));
    expect(await navigator.clipboard.readText()).toBe(EXTRA_TEXT);
  });

  it("names the server, the tool, the call's fingerprint and the one-call promise, with no scope picker", async () => {
    mockRows = [row()];
    renderScreen(false, true, "admin@acme.example");
    const card = await screen.findByTestId("mcp-call-card");
    expect(card).toHaveTextContent("jira · jira.example.com");
    expect(card).toHaveTextContent("mcp__jira__delete_issue");
    expect(screen.getByTestId("mcp-call-fingerprint")).toHaveTextContent(C.FINGERPRINT(KEY.slice(0, 12)));
    expect(card).toHaveTextContent(C.APPROVE_NOTE);
    expect(card).toHaveTextContent(C.DENY_NOTE);
    // One call, decided once: no scope menu, no reason dialog stop.
    for (const word of ["This run", "Until", "Always"]) expect(card).not.toHaveTextContent(word);
    expect(within(card).queryByRole("menu")).toBeNull();
    expect(within(card).getByRole("button", { name: C.APPROVE })).toBeEnabled();
  });

  it("gives the raw method its own line, quoted, so two tools with one display name read differently", async () => {
    mockRows = [{ ...row(), requested_scope: { ...row().requested_scope, mcp_tool: "delete.issue" } }];
    const { unmount } = renderScreen(false, true, "admin@acme.example");
    expect((await screen.findByTestId("mcp-call-raw-tool")).textContent).toBe('"delete.issue"');
    unmount();
    mockRows = [row()];
    renderScreen(false, true, "admin@acme.example");
    expect((await screen.findByTestId("mcp-call-raw-tool")).textContent).toBe('"delete_issue"');
  });

  it("draws names and arguments with control and bidi runes escaped, in the text and in the copy", async () => {
    const r = row();
    r.requested_scope = { ...r.requested_scope, mcp_tool: "x\u202eevil\u001b", mcp_server: "jira\u200b" };
    r.call_text = { arguments: '{\n  "note": "a\u202eb"\n}' };
    mockRows = [r];
    renderScreen(false, true, "admin@acme.example");
    const card = await screen.findByTestId("mcp-call-card");
    expect(card.textContent).not.toMatch(/[\u202e\u200b\u001b]/);
    // The raw method reads as the CLI prints it: quoted, with ESC as \x1b and the bidi mark as \u202e.
    expect(screen.getByTestId("mcp-call-raw-tool").textContent).toBe('"x\\u202eevil\\x1b"');
    expect(card).toHaveTextContent('"note": "a\\u202eb"');
    await userEvent.setup().click(within(card).getByRole("button", { name: C.COPY_ARGUMENTS }));
    expect(await navigator.clipboard.readText()).toBe('{\n  "note": "a\\u202eb"\n}');
  });

  it("shows and copies astral format runes as surrogate escapes, so the JSON still means the approved argument", async () => {
    const original = { args: '{\n  "note": "a\u{E0001}b"\n}', extra: '{\n  "tags": ["\u{1D173}"]\n}' };
    const r = row();
    r.call_text = { arguments: original.args, extra: original.extra };
    mockRows = [r];
    renderScreen(false, true, "admin@acme.example");
    const card = await screen.findByTestId("mcp-call-card");
    const shown = {
      args: within(card).getByRole("region", { name: C.ARGUMENTS }).textContent!,
      extra: within(card).getByRole("region", { name: C.EXTRA }).textContent!,
    };
    expect(JSON.parse(shown.args)).toEqual(JSON.parse(original.args));
    expect(JSON.parse(shown.extra)).toEqual(JSON.parse(original.extra));
    const user = userEvent.setup();
    await user.click(within(card).getByRole("button", { name: C.COPY_ARGUMENTS }));
    expect(await navigator.clipboard.readText()).toBe(shown.args);
    await user.click(within(card).getByRole("button", { name: C.COPY_EXTRA }));
    expect(await navigator.clipboard.readText()).toBe(shown.extra);
  });

  it("approves and denies straight through with no scope", async () => {
    mockRows = [row()];
    approveMock.mockResolvedValue(undefined);
    denyMock.mockResolvedValue(undefined);
    renderScreen(false, true, "admin@acme.example");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: C.APPROVE }));
    await waitFor(() => expect(approveMock).toHaveBeenCalledWith("apr_mcp", "approved"));
    await user.click(screen.getByRole("button", { name: C.DENY }));
    await waitFor(() => expect(denyMock).toHaveBeenCalledWith("apr_mcp", "denied"));
  });

  it("the run's owner decides it when the server says so; a person it does not name sees why not", async () => {
    mockRows = [row(', "decidable": true')];
    const { unmount } = renderScreen(false, false, "dana@acme.example");
    expect(await screen.findByRole("button", { name: C.APPROVE })).toBeEnabled();
    unmount();

    mockRows = [row(', "decidable": false')];
    renderScreen(false, false, "someone-else@acme.example");
    const card = await screen.findByTestId("mcp-call-card");
    expect(within(card).queryByRole("button", { name: C.APPROVE })).toBeNull();
    expect(card).toHaveTextContent(C.CANNOT_DECIDE);
  });

  it("a plain tool_call row is not given the owner's right to decide", async () => {
    mockRows = [{ id: "apr_t", run_id: "run_1", kind: "tool_call", requested_scope: { tool: "bash", cmd: "ls" }, state: "PENDING", requested_at: new Date().toISOString() }];
    renderScreen(false, false, "dana@acme.example");
    await screen.findByText("Tool call");
    // The generic card keeps its admin-only Approve: a member's own tool_call is not theirs to decide.
    expect(screen.getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(screen.queryByTestId("mcp-call-card")).toBeNull();
  });

  it("a row from a server with no call_text shows no arguments and offers Deny only", async () => {
    const r = row();
    delete r.call_text;
    mockRows = [r];
    renderScreen(false, true, "admin@acme.example");
    const card = await screen.findByTestId("mcp-call-card");
    expect(card).toHaveTextContent(C.NO_TEXT);
    expect(card.textContent).not.toMatch(/90071992547409/);
    expect(within(card).getByRole("button", { name: C.APPROVE })).toBeDisabled();
    expect(within(card).getByRole("button", { name: C.DENY })).toBeEnabled();
  });

  it("History names the tool and the server", async () => {
    mockRows = [{ ...row(), state: "APPROVED", decided_by: "admin@acme.example", decided_at: "2026-11-07T14:05:00Z", decision_scope: "once" }];
    renderScreen(false, true, "admin@acme.example", "/approvals?tab=decided");
    expect(await screen.findByText(C.LIST_TITLE("mcp__jira__delete_issue", "jira"))).toBeInTheDocument();
  });
});
